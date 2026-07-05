package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"dic/internal/compare"
	"dic/internal/diagram"
	"dic/internal/registry"
)

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed static/*
var staticFS embed.FS

// imageRow is one row in the unified image table: its slot/name, optional per-image error,
// and (once resolved) its size totals.
type imageRow struct {
	Slot      string
	Index     int // position in the canonical (insertion-order) list; used for removal
	Name      string
	Error     string // empty when the image resolved successfully
	Resolved  bool   // true when the image resolved and its Total is known
	Total     int64
	Shared    int64
	Unique    int64
	NumLayers int
	Created   time.Time // image creation time; zero if unknown
}

// HasDate reports whether a creation time is available to display.
func (r imageRow) HasDate() bool { return !r.Created.IsZero() }

// appView is the data passed to the "app" fragment template.
type appView struct {
	Rows         []imageRow // canonical, insertion order (drives hidden inputs, slots, state URL)
	SortedRows   []imageRow // same rows sorted by shared size, for the visible table
	CanAdd       bool
	ShowPlatform bool
	Platforms    []string
	Platform     string
	Result       *compare.Comparison
	Diagram      *diagram.Diagram // relationship diagram, when >= diagramMinImages are compared
	Error        string           // global error not attributable to a single image
	MaxImages    int              // the configured upper limit, for UI copy
}

// diagramMinImages is the image count at which the relationship diagram is shown.
const diagramMinImages = 3

// clientImage is image data resolved in the browser and posted back, letting the server
// skip its own registry fetch. Absent/partial data falls back to a server-side fetch.
type clientImage struct {
	Platforms []string         `json:"platforms"` // "os/arch[/variant]" available for the image
	Platform  string           `json:"platform"`  // platform the client fetched Layers for
	Layers    []registry.Layer `json:"layers"`
	Created   string           `json:"created"` // RFC3339 image creation time, if resolved
}

// parseClient decodes the optional "resolved" field (a JSON array of browser-resolved
// images) into a name-keyed map. Returns nil when absent or malformed (→ server fetches).
func parseClient(r *http.Request) map[string]clientImage {
	raw := r.FormValue("resolved")
	if raw == "" {
		return nil
	}
	var arr []struct {
		Name string `json:"name"`
		clientImage
	}
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return nil
	}
	m := make(map[string]clientImage, len(arr))
	for _, e := range arr {
		m[e.Name] = e.clientImage
	}
	return m
}

// slotLetter maps a zero-based index to its slot label (0 -> "A", 1 -> "B", ...).
func slotLetter(i int) string { return string(rune('A' + i)) }

// sortedByShared returns a copy of rows ordered by shared size, largest first, so the visible
// table leads with the most-overlapping images. The sort is stable, so uncompared/errored rows
// (shared == 0) keep their original relative order at the bottom. Each row keeps its slot
// letter and Index, so it still cross-references the diagram and removes correctly.
func sortedByShared(rows []imageRow) []imageRow {
	out := make([]imageRow, len(rows))
	copy(out, rows)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Shared > out[j].Shared })
	return out
}

type server struct {
	rc   *registry.Client
	tmpl *template.Template
}

func main() {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"humanSize": compare.HumanSize,
		"letter":    slotLetter,
		"reltime":   compare.RelativeAge,
		"isodate":   func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	}).ParseFS(tmplFS, "templates/*.html")
	if err != nil {
		log.Fatalf("parsing templates: %v", err)
	}

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}

	s := &server{rc: registry.NewClient(), tmpl: tmpl}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("POST /add", s.handleAdd)
	mux.HandleFunc("POST /remove", s.handleRemove)
	mux.HandleFunc("POST /compare", s.handleCompare)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("docker-image-compare listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// Reconstruct state from the URL so deep links and reloads work (server-side fetch).
	q := r.URL.Query()
	s.render(w, "index", s.buildView(reqCtx(r), q["image"], q.Get("platform"), nil))
}

func (s *server) handleAdd(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	images := r.Form["image"]
	// The modal textarea holds one image per line; append them all (buildView dedups).
	for _, line := range strings.Split(r.FormValue("newimage"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			images = append(images, line)
		}
	}
	s.renderFragment(w, s.buildView(reqCtx(r), images, r.FormValue("platform"), parseClient(r)))
}

func (s *server) handleRemove(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	images := r.Form["image"]
	if idx, err := strconv.Atoi(r.FormValue("remove")); err == nil && idx >= 0 && idx < len(images) {
		images = append(images[:idx:idx], images[idx+1:]...)
	}
	s.renderFragment(w, s.buildView(reqCtx(r), images, r.FormValue("platform"), parseClient(r)))
}

func (s *server) handleCompare(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	s.renderFragment(w, s.buildView(reqCtx(r), r.Form["image"], r.FormValue("platform"), parseClient(r)))
}

// renderFragment renders the "app" fragment and asks htmx to push a URL reflecting the
// current image list + platform, so the address bar is shareable and reload-safe.
func (s *server) renderFragment(w http.ResponseWriter, v appView) {
	w.Header().Set("HX-Push-Url", stateURL(v.Rows, v.Platform))
	s.render(w, "app", v)
}

// stateURL builds the canonical URL encoding the current images and platform. Invalid
// images are kept in the URL too, so a shared link reproduces the same (recoverable) state.
func stateURL(rows []imageRow, platform string) string {
	if len(rows) == 0 {
		return "/"
	}
	q := url.Values{}
	for _, r := range rows {
		q.Add("image", r.Name)
	}
	if platform != "" {
		q.Set("platform", platform)
	}
	return "/?" + q.Encode()
}

// maxImages is the most images that can be compared at once, from MAX_IMAGES (default 10).
var maxImages = envInt("MAX_IMAGES", 10)

// envInt reads a positive integer (>= 2) env var, falling back to def.
func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n >= 2 {
		return n
	}
	return def
}

// buildView resolves each image independently — a failure marks only that image's card as
// invalid (with its error and a retry affordance) rather than blocking the whole view. Each
// resolved image shows its own total size; the shared/unique split is computed only when at
// least two images resolve.
//
// When client holds browser-resolved data for an image, it is used in place of a registry
// fetch (offloading the server); any image absent from client (or whose data doesn't cover
// the chosen platform) falls back to a server-side fetch.
func (s *server) buildView(ctx context.Context, rawImages []string, selected string, client map[string]clientImage) (v appView) {
	var names []string
	seen := map[string]bool{}
	for _, im := range rawImages {
		if im = registry.NormalizeName(im); im == "" || seen[im] {
			continue // skip blanks and duplicates silently
		}
		seen[im] = true
		names = append(names, im)
		if len(names) == maxImages {
			break
		}
	}

	v = appView{CanAdd: len(names) < maxImages, Platform: selected, MaxImages: maxImages}
	if len(names) == 0 {
		return v
	}

	// Resolve every image; collect per-image errors instead of failing the request.
	type resolved struct {
		ref   registry.Ref
		plats []registry.Platform
		err   string
	}
	res := make([]resolved, len(names))
	for i, im := range names {
		ref, err := registry.ParseRef(im)
		if err != nil {
			res[i] = resolved{err: err.Error()}
			continue
		}
		if ci, ok := client[im]; ok && len(ci.Platforms) > 0 {
			res[i] = resolved{ref: ref, plats: registry.ParsePlatforms(ci.Platforms)}
			continue
		}
		plats, err := s.rc.Platforms(ctx, ref)
		if err != nil {
			res[i] = resolved{ref: ref, err: err.Error()}
			continue
		}
		res[i] = resolved{ref: ref, plats: plats}
	}

	v.Rows = make([]imageRow, len(names))
	for i, im := range names {
		v.Rows[i] = imageRow{Slot: slotLetter(i), Index: i, Name: im, Error: res[i].err}
	}
	defer func() { v.SortedRows = sortedByShared(v.Rows) }()

	// The valid subset drives platform selection and the comparison.
	var validIdx []int
	var validPlats [][]registry.Platform
	for i := range res {
		if res[i].err == "" {
			validIdx = append(validIdx, i)
			validPlats = append(validPlats, res[i].plats)
		}
	}
	if len(validIdx) == 0 {
		return v
	}

	common := registry.CommonPlatforms(validPlats)
	if len(common) == 0 {
		v.Error = "the valid images share no common platform to compare"
		return v
	}
	v.ShowPlatform = true
	for _, p := range common {
		v.Platforms = append(v.Platforms, p.String())
	}

	chosen := registry.PickPlatform(common, selected)
	v.Platform = chosen.String()

	// Gather layers for each valid image; a layer fetch failure invalidates just that card.
	// Prefer browser-resolved layers (when they cover the chosen platform) over a fetch.
	var comparedIdx []int
	var refStrings []string
	var layerSets [][]registry.Layer
	for _, i := range validIdx {
		var ls []registry.Layer
		if ci, ok := client[names[i]]; ok && ci.Platform == chosen.String() && len(ci.Layers) > 0 {
			ls = ci.Layers
			v.Rows[i].Created = registry.ParseTime(ci.Created)
		} else {
			fetched, created, err := s.rc.PlatformInfo(ctx, res[i].ref, chosen)
			if err != nil {
				v.Rows[i].Error = err.Error()
				continue
			}
			ls = fetched
			v.Rows[i].Created = created
		}
		comparedIdx = append(comparedIdx, i)
		refStrings = append(refStrings, res[i].ref.String())
		layerSets = append(layerSets, ls)
	}

	if len(layerSets) == 1 {
		// Only one image resolved: there's nothing to compare, but its total size
		// is still known. Show it and leave the shared/unique columns blank.
		cmp := compare.Images(refStrings, layerSets)
		v.Rows[comparedIdx[0]].Resolved = true
		v.Rows[comparedIdx[0]].Total = cmp.Images[0].Total
		v.Rows[comparedIdx[0]].NumLayers = cmp.Images[0].NumLayers
	} else if len(layerSets) >= 2 {
		cmp := compare.Images(refStrings, layerSets)
		for k, idx := range comparedIdx {
			v.Rows[idx].Resolved = true
			v.Rows[idx].Total = cmp.Images[k].Total
			v.Rows[idx].Shared = cmp.Images[k].Shared
			v.Rows[idx].Unique = cmp.Images[k].Unique
			v.Rows[idx].NumLayers = cmp.Images[k].NumLayers
		}
		v.Result = &cmp
		if len(comparedIdx) >= diagramMinImages {
			v.Diagram = diagram.Build(diagramInputs(comparedIdx, names, layerSets, cmp))
		}
	}
	return v
}

// diagramInputs assembles the self-contained per-image data the diagram package needs.
func diagramInputs(comparedIdx []int, names []string, layerSets [][]registry.Layer, cmp compare.Comparison) []diagram.Input {
	inputs := make([]diagram.Input, len(comparedIdx))
	for k, idx := range comparedIdx {
		layers := make([]diagram.Layer, len(layerSets[k]))
		for i, l := range layerSets[k] {
			layers[i] = diagram.Layer{Digest: l.Digest, Size: l.Size}
		}
		inputs[k] = diagram.Input{
			Slot:   slotLetter(idx),
			Name:   names[idx],
			Total:  cmp.Images[k].Total,
			Layers: layers,
		}
	}
	return inputs
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("template %q: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// reqCtx returns a context carrying the inbound client's forwarded address chain.
func reqCtx(r *http.Request) context.Context {
	return registry.WithForwardedFor(r.Context(), forwardedFor(r))
}

// forwardedFor builds the X-Forwarded-For value to propagate, appending the immediate peer.
func forwardedFor(r *http.Request) string {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
		return prior + ", " + ip
	}
	return ip
}
