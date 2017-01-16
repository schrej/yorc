package rest

import (
	"context"
	"encoding/json"
	"net"
	"net/http"

	"github.com/hashicorp/consul/api"
	"github.com/julienschmidt/httprouter"
	"github.com/justinas/alice"
	"github.com/pkg/errors"
	"novaforge.bull.com/starlings-janus/janus/config"
	"novaforge.bull.com/starlings-janus/janus/log"
	"novaforge.bull.com/starlings-janus/janus/tasks"
)

type router struct {
	*httprouter.Router
}

func (r *router) Get(path string, handler http.Handler) {
	r.GET(path, wrapHandler(handler))
}

func (r *router) Post(path string, handler http.Handler) {
	r.POST(path, wrapHandler(handler))
}

func (r *router) Put(path string, handler http.Handler) {
	r.PUT(path, wrapHandler(handler))
}

func (r *router) Delete(path string, handler http.Handler) {
	r.DELETE(path, wrapHandler(handler))
}

func (r *router) Head(path string, handler http.Handler) {
	r.HEAD(path, wrapHandler(handler))
}

func wrapHandler(h http.Handler) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		ctx := r.Context()
		h.ServeHTTP(w, r.WithContext(context.WithValue(ctx, "params", ps)))
	}
}

func NewRouter() *router {
	return &router{httprouter.New()}
}

type Server struct {
	router         *router
	listener       net.Listener
	consulClient   *api.Client
	tasksCollector *tasks.Collector
	config         config.Configuration
}

func (s *Server) Shutdown() {
	if s != nil {
		log.Printf("Shutting down http server")
		err := s.listener.Close()
		if err != nil {
			log.Print(err)
		}
	}
}

func NewServer(configuration config.Configuration, client *api.Client, shutdownCh chan struct{}) (*Server, error) {

	listener, err := net.Listen("tcp", ":8800")
	if err != nil {
		return nil, errors.Wrap(err, "Failed to bind on port 8800")
	}

	httpServer := &Server{
		router:         NewRouter(),
		listener:       listener,
		consulClient:   client,
		tasksCollector: tasks.NewCollector(client),
		config:         configuration,
	}

	httpServer.registerHandlers()
	log.Printf("Starting HTTPServer on port 8800")
	go http.Serve(httpServer.listener, httpServer.router)

	return httpServer, nil
}

func (s *Server) registerHandlers() {
	commonHandlers := alice.New(loggingHandler, recoverHandler)
	s.router.Post("/deployments", commonHandlers.Append(contentTypeHandler("application/zip")).ThenFunc(s.newDeploymentHandler))
	s.router.Delete("/deployments/:id", commonHandlers.ThenFunc(s.deleteDeploymentHandler))
	s.router.Get("/deployments/:id", commonHandlers.Append(acceptHandler("application/json")).ThenFunc(s.getDeploymentHandler))
	s.router.Get("/deployments", commonHandlers.Append(acceptHandler("application/json")).ThenFunc(s.listDeploymentsHandler))
	s.router.Get("/deployments/:id/events", commonHandlers.Append(acceptHandler("application/json")).ThenFunc(s.pollEvents))
	s.router.Head("/deployments/:id/events", commonHandlers.ThenFunc(s.headEventsIndex))
	s.router.Get("/deployments/:id/logs", commonHandlers.Append(acceptHandler("application/json")).ThenFunc(s.pollLogs))
	s.router.Head("/deployments/:id/logs", commonHandlers.ThenFunc(s.headLogsEventsIndex))
	s.router.Get("/deployments/:id/nodes/:nodeName", commonHandlers.Append(acceptHandler("application/json")).ThenFunc(s.getNodeHandler))
	s.router.Get("/deployments/:id/nodes/:nodeName/instances/:instanceId", commonHandlers.Append(acceptHandler("application/json")).ThenFunc(s.getNodeInstanceHandler))
	s.router.Get("/deployments/:id/outputs", commonHandlers.Append(acceptHandler("application/json")).ThenFunc(s.listOutputsHandler))
	s.router.Get("/deployments/:id/outputs/:opt", commonHandlers.Append(acceptHandler("application/json")).ThenFunc(s.getOutputHandler))
	s.router.Get("/deployments/:id/tasks/:taskId", commonHandlers.Append(acceptHandler("application/json")).ThenFunc(s.getTaskHandler))
	s.router.Delete("/deployments/:id/tasks/:taskId", commonHandlers.ThenFunc(s.cancelTaskHandler))
	s.router.Post("/deployments/:id/tasks", commonHandlers.Append(contentTypeHandler("application/json")).ThenFunc(s.newTaskHandler))
	s.router.Post("/deployments/:id/scale/:nodeName", commonHandlers.ThenFunc(s.scaleHandler))
	s.router.Get("/deployments/:id/nodes/:nodeName/instances/:instanceId/attributes", commonHandlers.Append(acceptHandler("application/json")).ThenFunc(s.getNodeInstanceAttributesListHandler))
	s.router.Get("/deployments/:id/nodes/:nodeName/instances/:instanceId/attributes/:attributeName", commonHandlers.Append(acceptHandler("application/json")).ThenFunc(s.getNodeInstanceAttributeHandler))
	s.router.Post("/deployments/:id/custom", commonHandlers.Append(contentTypeHandler("application/json")).ThenFunc(s.newCustomCommandHandler))
}

func encodeJSONResponse(w http.ResponseWriter, r *http.Request, resp interface{}) {
	jEnc := json.NewEncoder(w)
	if _, ok := r.URL.Query()["pretty"]; ok {
		jEnc.SetIndent("", "  ")
	}
	w.Header().Set("Content-Type", "application/json")
	jEnc.Encode(resp)
}
