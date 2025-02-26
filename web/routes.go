package web

import (
	"fmt"
	"html/template"
	"io/fs"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/amir20/dozzle/analytics"
	"github.com/amir20/dozzle/docker"

	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// Config is a struct for configuring the web service
type Config struct {
	Base        string
	Addr        string
	Version     string
	Username    string
	Password    string
	Hostname    string
	NoAnalytics bool
}

type handler struct {
	clients map[string]docker.Client
	content fs.FS
	config  *Config
}

// CreateServer creates a service for http handler
func CreateServer(clients map[string]docker.Client, content fs.FS, config Config) *http.Server {
	handler := &handler{
		clients: clients,
		content: content,
		config:  &config,
	}
	return &http.Server{Addr: config.Addr, Handler: createRouter(handler)}
}

var fileServer http.Handler

func createRouter(h *handler) *mux.Router {
	initializeAuth(h)

	base := h.config.Base
	r := mux.NewRouter()
	r.Use(cspHeaders)
	if base != "/" {
		r.HandleFunc(base, func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, base+"/", http.StatusMovedPermanently)
		})
	}
	s := r.PathPrefix(base).Subrouter()
	s.Handle("/api/logs/stream", authorizationRequired(h.streamLogs))
	s.Handle("/api/logs/download", authorizationRequired(h.downloadLogs))
	s.Handle("/api/logs", authorizationRequired(h.fetchLogsBetweenDates))
	s.Handle("/api/events/stream", authorizationRequired(h.streamEvents))
	s.HandleFunc("/api/validateCredentials", h.validateCredentials)
	s.Handle("/logout", authorizationRequired(h.clearSession))
	s.Handle("/version", authorizationRequired(h.version))
	s.HandleFunc("/healthcheck", h.healthcheck)

	if base != "/" {
		s.PathPrefix("/").Handler(http.StripPrefix(base+"/", http.HandlerFunc(h.index)))
	} else {
		s.PathPrefix("/").Handler(http.StripPrefix(base, http.HandlerFunc(h.index)))
	}

	fileServer = http.FileServer(http.FS(h.content))

	return r
}

func (h *handler) index(w http.ResponseWriter, req *http.Request) {
	_, err := h.content.Open(req.URL.Path)
	if err == nil && req.URL.Path != "" && req.URL.Path != "/" {
		fileServer.ServeHTTP(w, req)
		if !h.config.NoAnalytics {
			go func() {
				host, _ := os.Hostname()

				if containers, err := h.clients["localhost"].ListContainers(); err == nil {
					totalContainers := len(containers)
					runningContainers := 0
					for _, container := range containers {
						if container.State == "running" {
							runningContainers++
						}
					}

					re := analytics.RequestEvent{
						ClientId:          host,
						TotalContainers:   totalContainers,
						RunningContainers: runningContainers,
					}
					analytics.SendRequestEvent(re)
				}
			}()
		}
	} else {
		if !isAuthorized(req) && req.URL.Path != "login" {
			http.Redirect(w, req, path.Clean(h.config.Base+"/login"), http.StatusTemporaryRedirect)
			return
		}
		h.executeTemplate(w, req)
	}
}

func (h *handler) executeTemplate(w http.ResponseWriter, req *http.Request) {
	file, err := h.content.Open("index.html")
	if err != nil {
		log.Panic(err)
	}
	bytes, err := ioutil.ReadAll(file)
	if err != nil {
		log.Panic(err)
	}
	tmpl, err := template.New("index.html").Parse(string(bytes))
	if err != nil {
		log.Panic(err)
	}

	path := ""
	if h.config.Base != "/" {
		path = h.config.Base
	}

	// Get all keys from hosts map
	hosts := make([]string, 0, len(h.clients))
	for k := range h.clients {
		hosts = append(hosts, k)
	}

	data := struct {
		Base                string
		Version             string
		AuthorizationNeeded bool
		Secured             bool
		Hostname            string
		Hosts               string
	}{
		path,
		h.config.Version,
		h.isAuthorizationNeeded(req),
		secured,
		h.config.Hostname,
		strings.Join(hosts, ","),
	}
	err = tmpl.Execute(w, data)
	if err != nil {
		log.Panic(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (h *handler) version(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Content-Type", "text/html")
	fmt.Fprintf(w, "<pre>%v</pre>", h.config.Version)
}

func (h *handler) healthcheck(w http.ResponseWriter, r *http.Request) {
	log.Trace("Executing healthcheck request")

	if ping, err := h.clients["localhost"].Ping(r.Context()); err != nil {
		log.Error(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	} else {
		fmt.Fprintf(w, "OK API Version %v", ping.APIVersion)
	}
}

func (h *handler) clientFromRequest(r *http.Request) docker.Client {
	host := r.URL.Query().Get("host")
	if client, ok := h.clients[host]; ok {
		return client
	}
	return h.clients["localhost"]
}
