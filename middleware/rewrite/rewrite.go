package rewrite

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/kataras/iris/v12/context"
	"github.com/kataras/iris/v12/core/router"

	"gopkg.in/yaml.v3"
)

// Options holds the developer input to customize
// the redirects for the Rewrite Engine.
// Look the `New` and `Load` package-level functions.
type Options struct {
	// RedirectMatch accepts a slice of lines
	// of form:
	// REDIRECT_CODE PATH_PATTERN TARGET_PATH
	// Example: []{"301 /seo/(.*) /$1"}.
	RedirectMatch []string `json:"redirectMatch" yaml:"RedirectMatch"`

	// Root domain requests redirect automatically to primary subdomain.
	// Example: "www" to redirect always to www.
	// Note that you SHOULD NOT create a www subdomain inside the Iris Application.
	// This field takes care of it for you, the root application instance
	// will be used to serve the requests.
	PrimarySubdomain string `json:"primarySubdomain" yaml:"PrimarySubdomain"`
}

// LoadOptions loads rewrite Options from a system file.
func LoadOptions(filename string) (opts Options) {
	ext := ".yml"
	if index := strings.LastIndexByte(filename, '.'); index > 1 && len(filename)-1 > index {
		ext = filename[index:]
	}

	f, err := os.Open(filename)
	if err != nil {
		panic("iris: rewrite: " + err.Error())
	}
	defer f.Close()

	switch ext {
	case ".yaml", ".yml":
		err = yaml.NewDecoder(f).Decode(&opts)
	case ".json":
		err = json.NewDecoder(f).Decode(&opts)
	default:
		panic("iris: rewrite: unexpected file extension: " + filename)
	}

	if err != nil {
		panic("iris: rewrite: decode: " + err.Error())
	}

	return
}

// Engine is the rewrite engine master structure.
// Navigate through _examples/routing/rewrite for more.
type Engine struct {
	redirects []*redirectMatch
	options   Options

	domainValidator func(string) bool
}

// Load decodes the "filename" options
// and returns a new Rewrite Engine Router Wrapper.
// It panics on errors.
// Usage:
// redirects := Load("redirects.yml")
// app.WrapRouter(redirects)
// See `New` too.
func Load(filename string) router.WrapperFunc {
	opts := LoadOptions(filename)
	engine, err := New(opts)
	if err != nil {
		panic(err)
	}
	return engine.Rewrite
}

// New returns a new Rewrite Engine based on "opts".
// It reports any parser error.
// See its `Handler` or `Rewrite` methods. Depending
// on the needs, select one.
func New(opts Options) (*Engine, error) {
	redirects := make([]*redirectMatch, 0, len(opts.RedirectMatch))

	for _, line := range opts.RedirectMatch {
		r, err := parseRedirectMatchLine(line)
		if err != nil {
			return nil, err
		}
		redirects = append(redirects, r)
	}

	if opts.PrimarySubdomain != "" && !strings.HasSuffix(opts.PrimarySubdomain, ".") {
		opts.PrimarySubdomain += "." // www -> www.
	}

	e := &Engine{
		options:   opts,
		redirects: redirects,
		domainValidator: func(root string) bool {
			return !strings.HasSuffix(root, localhost)
		},
	}
	return e, nil
}

// Handler is an Iris Handler that can be used as a router or party or route middleware.
// For a global alternative, if you want to wrap the entire Iris Application
// use the `Wrapper` instead.
// Usage:
// app.UseRouter(engine.Handler)
func (e *Engine) Handler(ctx *context.Context) {
	e.Rewrite(ctx.ResponseWriter(), ctx.Request(), func(http.ResponseWriter, *http.Request) {
		ctx.Next()
	})
}

const localhost = "localhost"

// Rewrite is used to wrap the entire Iris Router.
// Rewrite is a bit faster than Handler because it's executed
// even before any route matched and it stops on redirect pattern match.
// Use it to wrap the entire Iris Application, otherwise look `Handler` instead.
//
// Usage:
// app.WrapRouter(engine.Rewrite).
func (e *Engine) Rewrite(w http.ResponseWriter, r *http.Request, routeHandler http.HandlerFunc) {
	if primarySubdomain := e.options.PrimarySubdomain; primarySubdomain != "" {
		hostport := context.GetHost(r)
		root := context.GetDomain(hostport)
		// Note:
		// localhost and 127.0.0.1 are not supported for subdomain rewrite, by purpose,
		// use a virtual host instead.
		// GetDomain will return will return localhost or www.localhost
		// on expected loopbacks.
		if e.domainValidator(root) {
			root += getPort(hostport)
			subdomain := strings.TrimSuffix(hostport, root)

			if subdomain == "" {
				// we are in root domain, full redirect to its primary subdomain.
				r.Host = primarySubdomain + root
				r.URL.Host = primarySubdomain + root
				http.Redirect(w, r, r.URL.String(), http.StatusMovedPermanently)
				return
			}

			if subdomain == primarySubdomain {
				// keep root domain as the Host field inside the next handlers,
				// for consistently use and
				// to bypass the subdomain router (`routeHandler`)
				// do not return, redirects should be respected.
				rootHost := strings.TrimPrefix(hostport, subdomain)

				// modify those for the next redirects or the route handler.
				r.Host = rootHost
				r.URL.Host = rootHost
			}

			// maybe other subdomain or not at all, let's continue.
		}
	}

	for _, rd := range e.redirects {
		src := r.URL.Path
		if !rd.isRelativePattern {
			// don't change the request, use a full redirect.
			src = context.GetScheme(r) + context.GetHost(r) + r.URL.RequestURI()
		}

		if target, ok := rd.matchAndReplace(src); ok {
			if target == src {
				routeHandler(w, r)
				return
			}

			if rd.noRedirect {
				u, err := r.URL.Parse(target)
				if err != nil {
					http.Error(w, err.Error(), http.StatusMisdirectedRequest)
					return
				}

				r.URL = u
				routeHandler(w, r)
				return
			}

			if !rd.isRelativePattern {
				// this performs better, no need to check query or host,
				// the uri already built.
				redirectAbs(w, r, target, rd.code)
			} else {
				http.Redirect(w, r, target, rd.code)
			}

			return
		}
	}

	routeHandler(w, r)
}

type redirectMatch struct {
	code    int
	pattern *regexp.Regexp
	target  string

	isRelativePattern bool
	noRedirect        bool
}

func (r *redirectMatch) matchAndReplace(src string) (string, bool) {
	if r.pattern.MatchString(src) {
		if match := r.pattern.ReplaceAllString(src, r.target); match != "" {
			return match, true
		}
	}

	return "", false
}

func parseRedirectMatchLine(s string) (*redirectMatch, error) {
	parts := strings.Split(strings.TrimSpace(s), " ")
	if len(parts) != 3 {
		return nil, fmt.Errorf("redirect match: invalid line: %s", s)
	}

	codeStr, pattern, target := parts[0], parts[1], parts[2]

	for i, ch := range codeStr {
		if !isDigit(ch) {
			return nil, fmt.Errorf("redirect match: status code digits: %s [%d:%c]", codeStr, i, ch)
		}
	}

	code, err := strconv.Atoi(codeStr)
	if err != nil {
		// this should not happen, we check abt digit
		// and correctly position the error too but handle it.
		return nil, fmt.Errorf("redirect match: status code digits: %s: %v", codeStr, err)
	}

	regex := regexp.MustCompile(pattern)
	if regex.MatchString(target) {
		return nil, fmt.Errorf("redirect match: loop detected: pattern: %s vs target: %s", pattern, target)
	}

	v := &redirectMatch{
		code:              code,
		pattern:           regex,
		target:            target,
		noRedirect:        code <= 0,
		isRelativePattern: pattern[0] == '/', // search by path.
	}

	return v, nil
}

func isDigit(ch rune) bool {
	return '0' <= ch && ch <= '9'
}

func getPort(hostport string) string { // returns :port, note that this is only called on non-loopbacks.
	if portIdx := strings.IndexByte(hostport, ':'); portIdx > 0 {
		return hostport[portIdx:]
	}

	return ""
}

func redirectAbs(w http.ResponseWriter, r *http.Request, url string, code int) {
	// part of net/http std library.
	h := w.Header()

	_, hadCT := h[context.ContentTypeHeaderKey]

	h.Set("Location", url)
	if !hadCT && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		h.Set(context.ContentTypeHeaderKey, "text/html; charset=utf-8")
	}
	w.WriteHeader(code)

	// Shouldn't send the body for POST or HEAD; that leaves GET.
	if !hadCT && r.Method == "GET" {
		body := "<a href=\"" + template.HTMLEscapeString(url) + "\">" + http.StatusText(code) + "</a>.\n"
		fmt.Fprintln(w, body)
	}
}
