package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/sourcegraph/log"

	"github.com/sourcegraph/sourcegraph/cmd/gitserver/server/internal/cacert"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/conf"
	"github.com/sourcegraph/sourcegraph/internal/gitserver/protocol"
	"github.com/sourcegraph/sourcegraph/internal/types"
	"github.com/sourcegraph/sourcegraph/internal/vcs"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

// GitDir is an absolute path to a GIT_DIR.
// They will all follow the form:
//
//    ${s.ReposDir}/${name}/.git
type GitDir string

// Path is a helper which returns filepath.Join(dir, elem...)
func (dir GitDir) Path(elem ...string) string {
	return filepath.Join(append([]string{string(dir)}, elem...)...)
}

// Set updates cmd so that it will run in dir.
//
// Note: GitDir is always a valid GIT_DIR, so we additionally set the
// environment variable GIT_DIR. This is to avoid git doing discovery in case
// of a bad repo, leading to hard to diagnose error messages.
func (dir GitDir) Set(cmd *exec.Cmd) {
	cmd.Dir = string(dir)
	if cmd.Env == nil {
		// Do not strip out existing env when setting.
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "GIT_DIR="+string(dir))
}

func (s *Server) dir(name api.RepoName) GitDir {
	path := string(protocol.NormalizeRepo(name))
	return GitDir(filepath.Join(s.ReposDir, filepath.FromSlash(path), ".git"))
}

func (s *Server) name(dir GitDir) api.RepoName {
	// dir == ${s.ReposDir}/${name}/.git
	parent := filepath.Dir(string(dir))                   // remove suffix "/.git"
	name := strings.TrimPrefix(parent, s.ReposDir)        // remove prefix "${s.ReposDir}"
	name = strings.Trim(name, string(filepath.Separator)) // remove /
	name = filepath.ToSlash(name)                         // filepath -> path
	return protocol.NormalizeRepo(api.RepoName(name))
}

func cloneStatus(cloned, cloning bool) types.CloneStatus {
	switch {
	case cloned:
		return types.CloneStatusCloned
	case cloning:
		return types.CloneStatusCloning
	}
	return types.CloneStatusNotCloned
}

func isAlwaysCloningTest(name api.RepoName) bool {
	return protocol.NormalizeRepo(name).Equal("github.com/sourcegraphtest/alwayscloningtest")
}

// checkSpecArgSafety returns a non-nil err if spec begins with a "-", which could
// cause it to be interpreted as a git command line argument.
func checkSpecArgSafety(spec string) error {
	if strings.HasPrefix(spec, "-") {
		return errors.Errorf("invalid git revision spec %q (begins with '-')", spec)
	}
	return nil
}

type tlsConfig struct {
	// Whether to not verify the SSL certificate when fetching or pushing over
	// HTTPS.
	//
	// https://git-scm.com/docs/git-config#Documentation/git-config.txt-httpsslVerify
	SSLNoVerify bool

	// File containing the certificates to verify the peer with when fetching
	// or pushing over HTTPS.
	//
	// https://git-scm.com/docs/git-config#Documentation/git-config.txt-httpsslCAInfo
	SSLCAInfo string
}

// getTlsExternalDoNotInvoke as the name suggests, exists as a function instead of being passed
// directly to conf.Cached below just so that we can test it.
func getTlsExternalDoNotInvoke() *tlsConfig {
	exp := conf.ExperimentalFeatures()
	c := exp.TlsExternal

	logger := log.Scoped("tlsExternal", "Global TLS/SSL settings for Sourcegraph to use when communicating with code hosts.")

	if c == nil {
		return &tlsConfig{}
	}

	sslCAInfo := ""
	if len(c.Certificates) > 0 {
		var b bytes.Buffer
		for _, cert := range c.Certificates {
			b.WriteString(cert)
			b.WriteString("\n")
		}

		// git will ignore the system certificates when specifying SSLCAInfo,
		// so we additionally include the system certificates. Note: this only
		// works on linux, see cacert package for more information.
		root, err := cacert.System()
		if err != nil {
			logger.Error("failed to load system certificates for inclusion in SSLCAInfo. Git will now fail to speak to TLS services not specified in your TlsExternal site configuration.", log.Error(err))
		} else if len(root) == 0 {
			logger.Warn("no system certificates found for inclusion in SSLCAInfo. Git will now fail to speak to TLS services not specified in your TlsExternal site configuration.")
		}
		for _, cert := range root {
			b.Write(cert)
			b.WriteString("\n")
		}

		// We don't clean up the file since it has a process life time.
		p, err := writeTempFile("gitserver*.crt", b.Bytes())
		if err != nil {
			logger.Error("failed to create file holding tls.external.certificates for git", log.Error(err))
		} else {
			sslCAInfo = p
		}
	}

	return &tlsConfig{
		SSLNoVerify: c.InsecureSkipVerify,
		SSLCAInfo:   sslCAInfo,
	}
}

// tlsExternal will create a new cache for this gitserer process and store the certificates set in
// the site config.
// This creates a long lived
var tlsExternal = conf.Cached(getTlsExternalDoNotInvoke)

// runWith runs the command after applying the remote options. If progress is not
// nil, all output is written to it in a separate goroutine.
func runWith(ctx context.Context, cmd *exec.Cmd, configRemoteOpts bool, progress io.Writer) ([]byte, error) {
	if configRemoteOpts {
		// Inherit process environment. This allows admins to configure
		// variables like http_proxy/etc.
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		configureRemoteGitCommand(cmd, tlsExternal())
	}

	var b interface {
		Bytes() []byte
	}

	logger := log.Scoped("runWith", "runWith runs the command after applying the remote options")

	if progress != nil {
		var pw progressWriter
		r, w := io.Pipe()
		defer w.Close()
		mr := io.MultiWriter(&pw, w)
		cmd.Stdout = mr
		cmd.Stderr = mr
		go func() {
			if _, err := io.Copy(progress, r); err != nil {
				logger.Error("error while copying progress", log.Error(err))
			}
		}()
		b = &pw
	} else {
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		b = &buf
	}

	_, err := runCommand(ctx, cmd)
	return b.Bytes(), err
}

func configureRemoteGitCommand(cmd *exec.Cmd, tlsConf *tlsConfig) {
	// We split here in case the first command is an absolute path to the executable
	// which allows us to safely match lower down
	_, executable := path.Split(cmd.Args[0])
	// As a special case we also support the experimental p4-fusion client which is
	// not run as a subcommand of git.
	if executable != "git" && executable != "p4-fusion" {
		panic(fmt.Sprintf("Only git or p4-fusion commands are supported, got %q", executable))
	}

	cmd.Env = append(cmd.Env, "GIT_ASKPASS=true") // disable password prompt

	// Suppress asking to add SSH host key to known_hosts (which will hang because
	// the command is non-interactive).
	//
	// And set a timeout to avoid indefinite hangs if the server is unreachable.
	cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=30")

	// Identify HTTP requests with a user agent. Please keep the git/ prefix because GitHub breaks the protocol v2
	// negotiation of clone URLs without a `.git` suffix (which we use) without it. Don't ask.
	cmd.Env = append(cmd.Env, "GIT_HTTP_USER_AGENT=git/Sourcegraph-Bot")

	if tlsConf.SSLNoVerify {
		cmd.Env = append(cmd.Env, "GIT_SSL_NO_VERIFY=true")
	}
	if tlsConf.SSLCAInfo != "" {
		cmd.Env = append(cmd.Env, "GIT_SSL_CAINFO="+tlsConf.SSLCAInfo)
	}

	extraArgs := []string{
		// Unset credential helper because the command is non-interactive.
		"-c", "credential.helper=",
	}

	if len(cmd.Args) > 1 && cmd.Args[1] != "ls-remote" {
		// Use Git protocol version 2 for all commands except for ls-remote because it actually decreases the performance of ls-remote.
		// https://opensource.googleblog.com/2018/05/introducing-git-protocol-version-2.html
		extraArgs = append(extraArgs, "-c", "protocol.version=2")
	}

	cmd.Args = append(cmd.Args[:1], append(extraArgs, cmd.Args[1:]...)...)
}

// writeTempFile writes data to the TempFile with pattern. Returns the path of
// the tempfile.
func writeTempFile(pattern string, data []byte) (path string, err error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}

	defer func() {
		if err1 := f.Close(); err == nil {
			err = err1
		}
		// Cleanup if we fail to write
		if err != nil {
			path = ""
			os.Remove(f.Name())
		}
	}()

	n, err := f.Write(data)
	if err == nil && n < len(data) {
		return "", io.ErrShortWrite
	}

	return f.Name(), err
}

// repoCloned checks if dir or `${dir}/.git` is a valid GIT_DIR.
var repoCloned = func(dir GitDir) bool {
	_, err := os.Stat(dir.Path("HEAD"))
	return !os.IsNotExist(err)
}

// repoLastFetched returns the mtime of the repo's FETCH_HEAD, which is the date of the last successful `git remote
// update` or `git fetch` (even if nothing new was fetched). As a special case when the repo has been cloned but
// none of those other two operations have been run (and so FETCH_HEAD does not exist), it will return the mtime of HEAD.
//
// This breaks on file systems that do not record mtime and if Git ever changes this undocumented behavior.
var repoLastFetched = func(dir GitDir) (time.Time, error) {
	fi, err := os.Stat(dir.Path("FETCH_HEAD"))
	if os.IsNotExist(err) {
		fi, err = os.Stat(dir.Path("HEAD"))
	}
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// repoLastChanged returns the mtime of the repo's sg_refhash, which is the
// cached timestamp of the most recent commit we could find in the tree. As a
// special case when sg_refhash is missing we return repoLastFetched(dir).
//
// This breaks on file systems that do not record mtime. This is a Sourcegraph
// extension to track last time a repo changed. The file is updated by
// setLastChanged via doBackgroundRepoUpdate.
//
// As a special case, tries both the directory given, and the .git subdirectory,
// because we're a bit inconsistent about which name to use.
var repoLastChanged = func(dir GitDir) (time.Time, error) {
	fi, err := os.Stat(dir.Path("sg_refhash"))
	if os.IsNotExist(err) {
		return repoLastFetched(dir)
	}
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// repoRemoteRefs returns a map containing ref + commit pairs from the
// remote Git repository starting with the specified prefix.
//
// The ref prefix `ref/<ref type>/` is stripped away from the returned
// refs.
var repoRemoteRefs = func(ctx context.Context, remoteURL *vcs.URL, prefix string) (map[string]string, error) {
	// The expected output of this git command is a list of:
	// <commit hash> <ref name>
	cmd := exec.Command("git", "ls-remote", remoteURL.String(), prefix+"*")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_, err := runCommand(ctx, cmd)
	if err != nil {
		stderr := stderr.Bytes()
		if len(stderr) > 200 {
			stderr = stderr[:200]
		}
		return nil, errors.Errorf("git %s failed: %s (%q)", cmd.Args, err, stderr)
	}

	refs := make(map[string]string)
	raw := stdout.String()
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, errors.Errorf("git %s failed (invalid output): %s", cmd.Args, line)
		}

		split := strings.SplitN(fields[1], "/", 3)
		if len(split) != 3 {
			return nil, errors.Errorf("git %s failed (invalid refname): %s", cmd.Args, fields[1])
		}

		refs[split[2]] = fields[0]
	}
	return refs, nil
}

// writeCounter wraps an io.Writer and keeps track of bytes written.
type writeCounter struct {
	w io.Writer
	// n is the number of bytes written to w
	n int64
}

func (c *writeCounter) Write(p []byte) (n int, err error) {
	n, err = c.w.Write(p)
	c.n += int64(n)
	return
}

// limitWriter is a io.Writer that writes to an W but discards after N bytes.
type limitWriter struct {
	W io.Writer // underling writer
	N int       // max bytes remaining
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.N <= 0 {
		return len(p), nil
	}
	origLen := len(p)
	if len(p) > l.N {
		p = p[:l.N]
	}
	n, err := l.W.Write(p)
	l.N -= n
	if l.N <= 0 {
		// If we have written limit bytes, then we can include the discarded
		// part of p in the count.
		n = origLen
	}
	return n, err
}

// flushingResponseWriter is a http.ResponseWriter that flushes all writes
// to the underlying connection within a certain time period after Write is
// called (instead of buffering them indefinitely).
//
// This lets, e.g., clients with a context deadline see as much partial response
// body as possible.
type flushingResponseWriter struct {
	// mu ensures we don't concurrently call Flush and Write. It also protects
	// state.
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	closed  bool
	doFlush bool
}

var logUnflushableResponseWriterOnce sync.Once

// newFlushingResponseWriter creates a new flushing response writer. Callers
// must call Close to free the resources created by the writer.
//
// If w does not support flushing, it returns nil.
func newFlushingResponseWriter(logger log.Logger, w http.ResponseWriter) *flushingResponseWriter {
	// We panic if we don't implement the needed interfaces.
	flusher := hackilyGetHTTPFlusher(w)
	if flusher == nil {
		logUnflushableResponseWriterOnce.Do(func() {
			logger.Warn("unable to flush HTTP response bodies - Diff search performance and completeness will be affected",
				log.String("type", reflect.TypeOf(w).String()))
		})
		return nil
	}

	w.Header().Set("Transfer-Encoding", "chunked")

	f := &flushingResponseWriter{w: w, flusher: flusher}
	go f.periodicFlush()
	return f
}

// hackilyGetHTTPFlusher attempts to get an http.Flusher from w. It (hackily) handles the case where w is a
// nethttp.statusCodeTracker (which wraps http.ResponseWriter and does not implement http.Flusher). See
// https://github.com/opentracing-contrib/go-stdlib/pull/11#discussion_r164295773 and
// https://github.com/sourcegraph/sourcegraph/issues/9045.
//
// I (@sqs) wrote this hack instead of fixing it upstream immediately because seems to be some reluctance to merge
// a fix (because it'd make the http.ResponseWriter falsely appear to implement many interfaces that it doesn't
// actually implement, so it would break the correctness of Go type-assertion impl checks).
func hackilyGetHTTPFlusher(w http.ResponseWriter) http.Flusher {
	if f, ok := w.(http.Flusher); ok {
		return f
	}
	if reflect.TypeOf(w).String() == "*nethttp.statusCodeTracker" {
		v := reflect.ValueOf(w).Elem()
		if v.Kind() == reflect.Struct {
			if rwv := v.FieldByName("ResponseWriter"); rwv.IsValid() {
				f, ok := rwv.Interface().(http.Flusher)
				if ok {
					return f
				}
			}
		}
	}
	return nil
}

// Header implements http.ResponseWriter.
func (f *flushingResponseWriter) Header() http.Header { return f.w.Header() }

// WriteHeader implements http.ResponseWriter.
func (f *flushingResponseWriter) WriteHeader(code int) { f.w.WriteHeader(code) }

// Write implements http.ResponseWriter.
func (f *flushingResponseWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	n, err := f.w.Write(p)
	if n > 0 {
		f.doFlush = true
	}
	f.mu.Unlock()
	return n, err
}

func (f *flushingResponseWriter) periodicFlush() {
	for {
		time.Sleep(100 * time.Millisecond)
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			break
		}
		if f.doFlush {
			f.flusher.Flush()
		}
		f.mu.Unlock()
	}
}

// Close signals to the flush goroutine to stop.
func (f *flushingResponseWriter) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

// progressWriter is an io.Writer that writes to a buffer.
// '\r' resets the write offset to the index after last '\n' in the buffer,
// or the beginning of the buffer if a '\n' has not been written yet.
type progressWriter struct {
	// writeOffset is the offset in buf where the next write should begin.
	writeOffset int

	// afterLastNewline is the index after the last '\n' in buf
	// or 0 if there is no '\n' in buf.
	afterLastNewline int

	buf []byte
}

func (w *progressWriter) Write(p []byte) (n int, err error) {
	l := len(p)
	for {
		if len(p) == 0 {
			// If p ends in a '\r' we still want to include that in the buffer until it is overwritten.
			break
		}
		idx := bytes.IndexAny(p, "\r\n")
		if idx == -1 {
			w.buf = append(w.buf[:w.writeOffset], p...)
			w.writeOffset = len(w.buf)
			break
		}
		switch p[idx] {
		case '\n':
			w.buf = append(w.buf[:w.writeOffset], p[:idx+1]...)
			w.writeOffset = len(w.buf)
			w.afterLastNewline = len(w.buf)
			p = p[idx+1:]
		case '\r':
			w.buf = append(w.buf[:w.writeOffset], p[:idx+1]...)
			// Record that our next write should overwrite the data after the most recent newline.
			// Don't slice it off immediately here, because we want to be able to return that output
			// until it is overwritten.
			w.writeOffset = w.afterLastNewline
			p = p[idx+1:]
		default:
			panic(fmt.Sprintf("unexpected char %q", p[idx]))
		}
	}
	return l, nil
}

// String returns the contents of the buffer as a string.
func (w *progressWriter) String() string {
	return string(w.buf)
}

// Bytes returns the contents of the buffer.
func (w *progressWriter) Bytes() []byte {
	return w.buf
}

// mapToLoggerField translates a map to log context fields.
func mapToLoggerField(m map[string]any) []log.Field {
	LogFields := []log.Field{}

	for i, v := range m {

		LogFields = append(LogFields, log.String(i, fmt.Sprint(v)))
	}

	return LogFields
}

// isPaused returns true if a file "SG_PAUSE" is present in dir. If the file is
// present, its first 40 bytes are returned as first argument.
func isPaused(dir string) (string, bool) {
	f, err := os.Open(filepath.Join(dir, "SG_PAUSE"))
	if err != nil {
		return "", false
	}
	defer f.Close()
	b := make([]byte, 40)
	io.ReadFull(f, b)
	return string(b), true
}

// bestEffortWalk is a filepath.Walk which ignores errors that can be passed
// to walkFn. This is a common pattern used in gitserver for best effort work.
//
// Note: We still respect errors returned by walkFn.
//
// filepath.Walk can return errors if we run into permission errors or a file
// disappears between readdir and the stat of the file. In either case this
// error can be ignored for best effort code.
func bestEffortWalk(root string, walkFn func(path string, info fs.FileInfo) error) error {
	logger := log.Scoped("bestEffortWalk", "bestEffortWalk is a filepath.Walk which ignores errors that can be passed to walkFn")
	return filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		if msg, ok := isPaused(path); ok {
			logger.Warn("bestEffortWalk paused", log.String("dir", path), log.String("reason", msg))
			return filepath.SkipDir
		}

		return walkFn(path, info)
	})
}
