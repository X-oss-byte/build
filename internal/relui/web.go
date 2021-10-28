// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package relui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v4/pgxpool"
	"golang.org/x/build/internal/relui/db"
	"golang.org/x/build/internal/workflow"
)

// fileServerHandler returns a http.Handler rooted at root. It will
// call the next handler provided for requests to "/".
//
// The returned handler sets the appropriate Content-Type and
// Cache-Control headers for the returned file.
func fileServerHandler(fs fs.FS, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(r.URL.Path)))
		w.Header().Set("Cache-Control", "no-cache, private, max-age=0")
		s := http.FileServer(http.FS(fs))
		s.ServeHTTP(w, r)
	})
}

var (
	homeTmpl        = template.Must(template.Must(layoutTmpl.Clone()).ParseFS(templates, "templates/home.html"))
	layoutTmpl      = template.Must(template.ParseFS(templates, "templates/layout.html"))
	newWorkflowTmpl = template.Must(template.Must(layoutTmpl.Clone()).ParseFS(templates, "templates/new_workflow.html"))
)

// Server implements the http handlers for relui.
type Server struct {
	db *pgxpool.Pool
	m  *http.ServeMux
	w  *Worker
}

// NewServer initializes a server with the provided connection pool.
func NewServer(p *pgxpool.Pool, w *Worker) *Server {
	s := &Server{
		db: p,
		m:  new(http.ServeMux),
		w:  w,
	}
	s.m.Handle("/workflows/create", http.HandlerFunc(s.createWorkflowHandler))
	s.m.Handle("/workflows/new", http.HandlerFunc(s.newWorkflowHandler))
	s.m.Handle("/", fileServerHandler(static, http.HandlerFunc(s.homeHandler)))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.m.ServeHTTP(w, r)
}

func (s *Server) Serve(port string) error {
	return http.ListenAndServe(":"+port, s.m)
}

type homeResponse struct {
	Workflows     []db.Workflow
	WorkflowTasks map[uuid.UUID][]db.Task
	TaskLogs      map[uuid.UUID]map[string][]db.TaskLog
}

func (h *homeResponse) Logs(workflow uuid.UUID, task string) []db.TaskLog {
	t := h.TaskLogs[workflow]
	if t == nil {
		return nil
	}
	return t[task]
}

func (h *homeResponse) WorkflowParams(wf db.Workflow) map[string]string {
	params := make(map[string]string)
	json.Unmarshal([]byte(wf.Params.String), &params)
	return params
}

// homeHandler renders the homepage.
func (s *Server) homeHandler(w http.ResponseWriter, r *http.Request) {
	resp, err := s.buildHomeResponse(r.Context())
	if err != nil {
		log.Printf("homeHandler: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	out := bytes.Buffer{}
	if err := homeTmpl.Execute(&out, resp); err != nil {
		log.Printf("homeHandler: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	io.Copy(w, &out)
}

func (s *Server) buildHomeResponse(ctx context.Context) (*homeResponse, error) {
	q := db.New(s.db)
	ws, err := q.Workflows(ctx)
	if err != nil {
		return nil, err
	}
	tasks, err := q.Tasks(ctx)
	if err != nil {
		return nil, err
	}
	wfTasks := make(map[uuid.UUID][]db.Task, len(ws))
	for _, t := range tasks {
		wfTasks[t.WorkflowID] = append(wfTasks[t.WorkflowID], t)
	}
	tlogs, err := q.TaskLogs(ctx)
	if err != nil {
		return nil, err
	}
	wftlogs := make(map[uuid.UUID]map[string][]db.TaskLog)
	for _, l := range tlogs {
		if wftlogs[l.WorkflowID] == nil {
			wftlogs[l.WorkflowID] = make(map[string][]db.TaskLog)
		}
		wftlogs[l.WorkflowID][l.TaskName] = append(wftlogs[l.WorkflowID][l.TaskName], l)
	}
	return &homeResponse{Workflows: ws, WorkflowTasks: wfTasks, TaskLogs: wftlogs}, nil
}

type newWorkflowResponse struct {
	Definitions map[string]*workflow.Definition
	Name        string
}

func (n *newWorkflowResponse) Selected() *workflow.Definition {
	return n.Definitions[n.Name]
}

// newWorkflowHandler presents a form for creating a new workflow.
func (s *Server) newWorkflowHandler(w http.ResponseWriter, r *http.Request) {
	out := bytes.Buffer{}
	resp := &newWorkflowResponse{
		Definitions: Definitions(),
		Name:        r.FormValue("workflow.name"),
	}
	if err := newWorkflowTmpl.Execute(&out, resp); err != nil {
		log.Printf("newWorkflowHandler: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	io.Copy(w, &out)
}

// createWorkflowHandler persists a new workflow in the datastore, and
// starts the workflow in a goroutine.
func (s *Server) createWorkflowHandler(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("workflow.name")
	d := Definition(name)
	if d == nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	params := make(map[string]string)
	for _, n := range d.ParameterNames() {
		params[n] = r.FormValue(fmt.Sprintf("workflow.params.%s", n))
		if params[n] == "" {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
	}
	if _, err := s.w.StartWorkflow(r.Context(), name, d, params); err != nil {
		log.Printf("s.w.StartWorkflow(%v, %v, %v): %v", r.Context(), d, params, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}