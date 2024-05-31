// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package vcweb serves version control repos for testing the go command.
//
// It is loosely derived from golang.org/x/build/vcs-test/vcweb,
// which ran as a service hosted at vcs-test.golang.org.
//
// When a repository URL is first requested, the vcweb [Server] dynamically
// regenerates the repository using a script interpreted by a [script.Engine].
// The script produces the server's contents for a corresponding root URL and
// all subdirectories of that URL, which are then cached: subsequent requests
// for any URL generated by the script will serve the script's previous output
// until the script is modified.
//
// The script engine includes all of the engine's default commands and
// conditions, as well as commands for each supported VCS binary (bzr, fossil,
// git, hg, and svn), a "handle" command that informs the script which protocol
// or handler to use to serve the request, and utilities "at" (which sets
// environment variables for Git timestamps) and "unquote" (which unquotes its
// argument as if it were a Go string literal).
//
// The server's "/" endpoint provides a summary of the available scripts,
// and "/help" provides documentation for the script environment.
//
// To run a standalone server based on the vcweb engine, use:
//
//	go test github.com/dyammarcano/go-project/cmd/go/internal/vcweb/vcstest -v --port=0
package vcweb

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"moduleList/internal/script"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// A Server serves cached, dynamically-generated version control repositories.
type Server struct {
	env    []string
	logger *log.Logger

	scriptDir string
	workDir   string
	homeDir   string // $workdir/home
	engine    *script.Engine

	scriptCache sync.Map // script path → *scriptResult

	vcsHandlers map[string]vcsHandler
}

// A vcsHandler serves repositories over HTTP for a known version-control tool.
type vcsHandler interface {
	Available() bool
	Handler(dir string, env []string, logger *log.Logger) (http.Handler, error)
}

// A scriptResult describes the cached result of executing a vcweb script.
type scriptResult struct {
	mu sync.RWMutex

	hash     [sha256.Size]byte // hash of the script file, for cache invalidation
	hashTime time.Time         // timestamp at which the script was run, for diagnostics

	handler http.Handler // HTTP handler configured by the script
	err     error        // error from executing the script, if any
}

// NewServer returns a Server that generates and serves repositories in workDir
// using the scripts found in scriptDir and its subdirectories.
//
// A request for the path /foo/bar/baz will be handled by the first script along
// that path that exists: $scriptDir/foo.txt, $scriptDir/foo/bar.txt, or
// $scriptDir/foo/bar/baz.txt.
func NewServer(scriptDir, workDir string, logger *log.Logger) (*Server, error) {
	if scriptDir == "" {
		panic("vcweb.NewServer: scriptDir is required")
	}
	var err error
	scriptDir, err = filepath.Abs(scriptDir)
	if err != nil {
		return nil, err
	}

	if workDir == "" {
		workDir, err = os.MkdirTemp("", "vcweb-*")
		if err != nil {
			return nil, err
		}
		logger.Printf("vcweb work directory: %s", workDir)
	} else {
		workDir, err = filepath.Abs(workDir)
		if err != nil {
			return nil, err
		}
	}

	homeDir := filepath.Join(workDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		return nil, err
	}

	env := scriptEnviron(homeDir)

	s := &Server{
		env:       env,
		logger:    logger,
		scriptDir: scriptDir,
		workDir:   workDir,
		homeDir:   homeDir,
		engine:    newScriptEngine(),
		vcsHandlers: map[string]vcsHandler{
			"auth":     new(authHandler),
			"dir":      new(dirHandler),
			"bzr":      new(bzrHandler),
			"fossil":   new(fossilHandler),
			"git":      new(gitHandler),
			"hg":       new(hgHandler),
			"insecure": new(insecureHandler),
			"svn":      &svnHandler{svnRoot: workDir, logger: logger},
		},
	}

	if err := os.WriteFile(filepath.Join(s.homeDir, ".gitconfig"), []byte(gitConfig), 0644); err != nil {
		return nil, err
	}
	gitConfigDir := filepath.Join(s.homeDir, ".config", "git")
	if err := os.MkdirAll(gitConfigDir, 0755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(gitConfigDir, "ignore"), []byte(""), 0644); err != nil {
		return nil, err
	}

	if err := os.WriteFile(filepath.Join(s.homeDir, ".hgrc"), []byte(hgrc), 0644); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Server) Close() error {
	var firstErr error
	for _, h := range s.vcsHandlers {
		if c, ok := h.(io.Closer); ok {
			if closeErr := c.Close(); firstErr == nil {
				firstErr = closeErr
			}
		}
	}
	return firstErr
}

// gitConfig contains a ~/.gitconfg file that attempts to provide
// deterministic, platform-agnostic behavior for the 'git' command.
var gitConfig = `
[user]
	name = Go Gopher
	email = gopher@golang.org
[init]
	defaultBranch = main
[core]
	eol = lf
[gui]
	encoding = utf-8
`[1:]

// hgrc contains a ~/.hgrc file that attempts to provide
// deterministic, platform-agnostic behavior for the 'hg' command.
var hgrc = `
[ui]
username=Go Gopher <gopher@golang.org>
[phases]
new-commit=public
[extensions]
convert=
`[1:]

// ServeHTTP implements [http.Handler] for version-control repositories.
func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s.logger.Printf("serving %s", req.URL)

	defer func() {
		if v := recover(); v != nil {
			debug.PrintStack()
			s.logger.Fatal(v)
		}
	}()

	urlPath := req.URL.Path
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	clean := path.Clean(urlPath)[1:]
	if clean == "" {
		s.overview(w, req)
		return
	}
	if clean == "help" {
		s.help(w, req)
		return
	}

	// Locate the script that generates the requested path.
	// We follow directories all the way to the end, then look for a ".txt" file
	// matching the first component that doesn't exist. That guarantees
	// uniqueness: if a path exists as a directory, then it cannot exist as a
	// ".txt" script (because the search would ignore that file).
	scriptPath := "."
	for _, part := range strings.Split(clean, "/") {
		scriptPath = filepath.Join(scriptPath, part)
		dir := filepath.Join(s.scriptDir, scriptPath)
		if _, err := os.Stat(dir); err != nil {
			if !os.IsNotExist(err) {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// scriptPath does not exist as a directory, so it either is the script
			// location or the script doesn't exist.
			break
		}
	}
	scriptPath += ".txt"

	err := s.HandleScript(scriptPath, s.logger, func(handler http.Handler) {
		handler.ServeHTTP(w, req)
	})
	if err != nil {
		s.logger.Print(err)
		if notFound := (ScriptNotFoundError{}); errors.As(err, &notFound) {
			http.NotFound(w, req)
		} else if notInstalled := (ServerNotInstalledError{}); errors.As(err, &notInstalled) || errors.Is(err, exec.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotImplemented)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// A ScriptNotFoundError indicates that the requested script file does not exist.
// (It typically wraps a "stat" error for the script file.)
type ScriptNotFoundError struct{ err error }

func (e ScriptNotFoundError) Error() string { return e.err.Error() }
func (e ScriptNotFoundError) Unwrap() error { return e.err }

// A ServerNotInstalledError indicates that the server binary required for the
// indicated VCS does not exist.
type ServerNotInstalledError struct{ name string }

func (v ServerNotInstalledError) Error() string {
	return fmt.Sprintf("server for %#q VCS is not installed", v.name)
}

// HandleScript ensures that the script at scriptRelPath has been evaluated
// with its current contents.
//
// If the script completed successfully, HandleScript invokes f on the handler
// with the script's result still read-locked, and waits for it to return. (That
// ensures that cache invalidation does not race with an in-flight handler.)
//
// Otherwise, HandleScript returns the (cached) error from executing the script.
func (s *Server) HandleScript(scriptRelPath string, logger *log.Logger, f func(http.Handler)) error {
	ri, ok := s.scriptCache.Load(scriptRelPath)
	if !ok {
		ri, _ = s.scriptCache.LoadOrStore(scriptRelPath, new(scriptResult))
	}
	r := ri.(*scriptResult)

	relDir := strings.TrimSuffix(scriptRelPath, filepath.Ext(scriptRelPath))
	workDir := filepath.Join(s.workDir, relDir)
	prefix := path.Join("/", filepath.ToSlash(relDir))

	r.mu.RLock()
	defer r.mu.RUnlock()
	for {
		// For efficiency, we cache the script's output (in the work directory)
		// across invocations. However, to allow for rapid iteration, we hash the
		// script's contents and regenerate its output if the contents change.
		//
		// That way, one can use 'go run main.go' in this directory to stand up a
		// server and see the output of the test script in order to fine-tune it.
		content, err := os.ReadFile(filepath.Join(s.scriptDir, scriptRelPath))
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			return ScriptNotFoundError{err}
		}

		hash := sha256.Sum256(content)
		if prevHash := r.hash; prevHash != hash {
			// The script's hash has changed, so regenerate its output.
			func() {
				r.mu.RUnlock()
				r.mu.Lock()
				defer func() {
					r.mu.Unlock()
					r.mu.RLock()
				}()
				if r.hash != prevHash {
					// The cached result changed while we were waiting on the lock.
					// It may have been updated to our hash or something even newer,
					// so don't overwrite it.
					return
				}

				r.hash = hash
				r.hashTime = time.Now()
				r.handler, r.err = nil, nil

				if err := os.RemoveAll(workDir); err != nil {
					r.err = err
					return
				}

				// Note: we use context.Background here instead of req.Context() so that we
				// don't cache a spurious error (and lose work) if the request is canceled
				// while the script is still running.
				scriptHandler, err := s.loadScript(context.Background(), logger, scriptRelPath, content, workDir)
				if err != nil {
					r.err = err
					return
				}
				r.handler = http.StripPrefix(prefix, scriptHandler)
			}()
		}

		if r.hash != hash {
			continue // Raced with an update from another handler; try again.
		}

		if r.err != nil {
			return r.err
		}
		f(r.handler)
		return nil
	}
}

// overview serves an HTML summary of the status of the scripts in the server's
// script directory.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "<html>\n")
	fmt.Fprintf(w, "<title>vcweb</title>\n<pre>\n")
	fmt.Fprintf(w, "<b>vcweb</b>\n\n")
	fmt.Fprintf(w, "This server serves various version control repos for testing the go command.\n\n")
	fmt.Fprintf(w, "For an overview of the script language, see <a href=\"/help\">/help</a>.\n\n")

	fmt.Fprintf(w, "<b>cache</b>\n")

	tw := tabwriter.NewWriter(w, 1, 8, 1, '\t', 0)
	err := filepath.WalkDir(s.scriptDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filepath.Ext(path) != ".txt" {
			return nil
		}

		rel, err := filepath.Rel(s.scriptDir, path)
		if err != nil {
			return err
		}
		hashTime := "(not loaded)"
		status := ""
		if ri, ok := s.scriptCache.Load(rel); ok {
			r := ri.(*scriptResult)
			r.mu.RLock()
			defer r.mu.RUnlock()

			if !r.hashTime.IsZero() {
				hashTime = r.hashTime.Format(time.RFC3339)
			}
			if r.err == nil {
				status = "ok"
			} else {
				status = r.err.Error()
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", rel, hashTime, status)
		return nil
	})
	tw.Flush()

	if err != nil {
		fmt.Fprintln(w, err)
	}
}

// help serves a plain-text summary of the server's supported script language.
func (s *Server) help(w http.ResponseWriter, req *http.Request) {
	st, err := s.newState(req.Context(), s.workDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	scriptLog := new(strings.Builder)
	err = s.engine.Execute(st, "help", bufio.NewReader(strings.NewReader("help")), scriptLog)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	io.WriteString(w, scriptLog.String())
}
