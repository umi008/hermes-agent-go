// Package web ports upstream web_search and web_extract
// (tools/web_tools.py) with the Tavily backend (plugins/web/tavily).
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

// Upstream limits.
const (
	DefaultBaseURL     = "https://api.tavily.com"
	searchLimitCap     = 20
	maxExtractURLs     = 5
	defaultCharLimit   = 15_000
	charLimitFloor     = 2_000
	charLimitCeiling   = 500_000
	requestTimeout     = 60 * time.Second
	clientName         = "hermes-agent"
	missingKeyTemplate = "TAVILY_API_KEY is not set. Get a key at https://app.tavily.com/home or select Tavily in `hermes tools` for opt-in keyless %s."
)

// Options configure the web tools.
type Options struct {
	APIKey  string // TAVILY_API_KEY; the tools are unavailable without it
	BaseURL string // TAVILY_BASE_URL; default DefaultBaseURL
	Client  *http.Client
	// CacheDir stores the full text of truncated pages; empty disables it.
	CacheDir string
	// LookupIP resolves hosts for the private-network check.
	LookupIP func(ctx context.Context, host string) ([]netip.Addr, error)
	Logger   *slog.Logger
}

// Web holds the Tavily client.
type Web struct{ o Options }

// New builds the web tools.
func New(o Options) *Web {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: requestTimeout}
	}
	if o.LookupIP == nil {
		o.LookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Web{o: o}
}

// Tools returns web_search and web_extract.
func (w *Web) Tools() []tools.Tool {
	avail := func() bool { return w.o.APIKey != "" }
	return []tools.Tool{{
		Name:        "web_search",
		Toolset:     "web",
		Description: "Search the web: returns titles, URLs and snippets (5 by default). Read pages with web_extract. The query is passed through to the configured backend, so operators such as site:domain, filetype:pdf, intitle:word, -term, and \"exact phrase\" may work when the backend supports them.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"query":{"type":"string","description":"The search query to look up on the web. You may include backend-supported operators such as site:example.com, filetype:pdf, intitle:word, -term, or \"exact phrase\"."},` +
			`"limit":{"type":"integer","description":"Maximum number of results to return. Defaults to 5.","minimum":1,"maximum":100,"default":5}` +
			`},"required":["query"]}`),
		Handler:   w.search,
		Available: avail,
	}, {
		Name:        "web_extract",
		Toolset:     "web",
		Description: "Extract content from web page URLs. Returns clean page content in markdown/text (no LLM summarization — fast). Also works with PDF URLs (arxiv papers, documents) — pass the PDF link directly. Pages within the char budget (default 15000) return whole; larger pages return a head+tail window with a footer telling you the full text's saved file path and the read_file call to page through the omitted middle. Inline images appear as [IMAGE: alt] placeholders; real image URLs are kept as links. If a URL fails, times out, or needs JS, login or clicks, use a browser tool instead.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"urls":{"type":"array","items":{"type":"string"},"description":"List of URLs to extract content from (max 5 URLs per call)","maxItems":5},` +
			`"char_limit":{"type":"integer","description":"Optional per-page character budget sent back (default 15000). Pages larger than this are head+tail truncated with the full text stored to disk. Raise it when you need more of a long page inline.","minimum":2000}` +
			`},"required":["urls"]}`),
		Handler:   w.extract,
		Available: avail,
	}}
}

// post sends a Tavily request. A >=400 reply returns its body as the error,
// so rate-limit and upgrade text reaches the model.
func (w *Web) post(ctx context.Context, endpoint string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(w.o.BaseURL, "/")+"/"+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Name", clientName)
	req.Header.Set("Authorization", "Bearer "+w.o.APIKey)
	resp, err := w.o.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		if msg := strings.TrimSpace(string(data)); msg != "" {
			return verbatimError(msg)
		}
		return verbatimError(fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	return json.Unmarshal(data, out)
}

// verbatimError is shown to the model as-is (upstream ValueError).
type verbatimError string

func (e verbatimError) Error() string { return string(e) }

func failure(kind string, err error) string {
	var v verbatimError
	if errors.As(err, &v) {
		return string(v)
	}
	return "Tavily " + kind + " failed: " + err.Error()
}

type hit struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Position    int    `json:"position"`
}

func (w *Web) search(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Query string          `json:"query"`
		Limit json.RawMessage `json:"limit"`
	}
	if err := tools.Decode(raw, &a); err != nil {
		return "", err
	}
	limit := 5
	if len(a.Limit) > 0 {
		var f float64
		if json.Unmarshal(a.Limit, &f) == nil {
			limit = min(max(int(f), 1), 100)
		}
	}
	fail := func(msg string) (string, error) {
		return tools.Result(struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		}{false, msg})
	}
	if w.o.APIKey == "" {
		return fail(fmt.Sprintf(missingKeyTemplate, "search"))
	}
	w.o.Logger.Info("web search", "query", a.Query, "limit", limit)
	var resp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	payload := map[string]any{"query": a.Query, "max_results": min(limit, searchLimitCap), "include_raw_content": false, "include_images": false}
	if err := w.post(ctx, "search", payload, &resp); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return fail(failure("search", err))
	}
	hits := make([]hit, 0, len(resp.Results))
	for i, r := range resp.Results {
		hits = append(hits, hit{Title: r.Title, URL: r.URL, Description: r.Content, Position: i + 1})
	}
	return tools.Result(struct {
		Success bool             `json:"success"`
		Data    map[string][]hit `json:"data"`
	}{true, map[string][]hit{"web": hits}})
}

type page struct {
	URL     string  `json:"url"`
	Title   string  `json:"title"`
	Content string  `json:"content"`
	Error   *string `json:"error"`
}

func errPage(u, msg string) page { return page{URL: u, Error: &msg} }

func (w *Web) extract(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		URLs      []json.RawMessage `json:"urls"`
		CharLimit *float64          `json:"char_limit"`
	}
	if err := tools.Decode(raw, &a); err != nil {
		return "", err
	}
	if len(a.URLs) > maxExtractURLs {
		a.URLs = a.URLs[:maxExtractURLs]
	}
	limit := defaultCharLimit
	if a.CharLimit != nil {
		limit = max(charLimitFloor, min(int(*a.CharLimit), charLimitCeiling))
	}
	results := make([]page, len(a.URLs))
	var fetch []string
	var fetchAt []int
	for i, item := range a.URLs {
		u := itemURL(item)
		if u == "" {
			results[i] = errPage("", fmt.Sprintf("Invalid URL item at index %d: expected a URL string or an object with a string 'url' or 'href' field", i))
			continue
		}
		if !w.safeURL(ctx, u) {
			results[i] = errPage(u, "Blocked: URL targets a private or internal network address")
			continue
		}
		fetch, fetchAt = append(fetch, u), append(fetchAt, i)
	}
	if len(fetch) > 0 {
		pages := w.fetchPages(ctx, fetch)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Tavily may reorder or drop URLs: match by URL, then fall back to
		// position, and report anything missing.
		byURL := map[string]page{}
		for _, p := range pages {
			byURL[p.URL] = p
		}
		for k, u := range fetch {
			p, ok := byURL[u]
			if !ok && k < len(pages) && pages[k].URL == "" {
				p, ok = pages[k], true
			}
			if !ok {
				p = errPage(u, "extraction failed")
			}
			results[fetchAt[k]] = p
		}
	}
	if len(results) == 0 {
		return "", tools.Error("Content was inaccessible or not found")
	}
	for i := range results {
		if results[i].Error == nil && results[i].Content != "" {
			results[i].Content = w.truncate(results[i].Content, results[i].URL, limit)
		}
	}
	return tools.Result(map[string]any{"results": results})
}

// itemURL accepts a URL string or an object with a url or href field.
func itemURL(item json.RawMessage) string {
	var u string
	if json.Unmarshal(item, &u) != nil {
		var obj struct {
			URL  *string `json:"url"`
			Href *string `json:"href"`
		}
		if json.Unmarshal(item, &obj) != nil {
			return ""
		}
		switch {
		case obj.URL != nil:
			u = *obj.URL
		case obj.Href != nil:
			u = *obj.Href
		}
	}
	return strings.TrimSpace(u)
}

func (w *Web) fetchPages(ctx context.Context, urls []string) []page {
	if w.o.APIKey == "" {
		out := make([]page, len(urls))
		for i, u := range urls {
			out[i] = errPage(u, fmt.Sprintf(missingKeyTemplate, "extract"))
		}
		return out
	}
	var resp struct {
		Results []struct {
			URL        string `json:"url"`
			Title      string `json:"title"`
			RawContent string `json:"raw_content"`
			Content    string `json:"content"`
		} `json:"results"`
		Failed []struct {
			URL   string `json:"url"`
			Error string `json:"error"`
		} `json:"failed_results"`
		FailedURLs []string `json:"failed_urls"`
	}
	if err := w.post(ctx, "extract", map[string]any{"urls": urls, "include_images": false}, &resp); err != nil {
		msg := failure("extract", err)
		out := make([]page, len(urls))
		for i, u := range urls {
			out[i] = errPage(u, msg)
		}
		return out
	}
	var out []page
	for _, r := range resp.Results {
		content := r.RawContent
		if content == "" {
			content = r.Content
		}
		out = append(out, page{URL: r.URL, Title: r.Title, Content: content})
	}
	for _, f := range resp.Failed {
		msg := f.Error
		if msg == "" {
			msg = "extraction failed"
		}
		out = append(out, errPage(f.URL, msg))
	}
	for _, u := range resp.FailedURLs {
		out = append(out, errPage(u, "extraction failed"))
	}
	return out
}

// safeURL rejects non-http(s) URLs and hosts that resolve to loopback,
// private, link-local or otherwise internal addresses (SSRF guard).
func (w *Web) safeURL(ctx context.Context, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return false
	}
	host := u.Hostname()
	addrs := []netip.Addr{}
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = append(addrs, a)
	} else {
		if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
			return false
		}
		if addrs, err = w.o.LookupIP(ctx, host); err != nil || len(addrs) == 0 {
			return false
		}
	}
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	for _, a := range addrs {
		a = a.Unmap()
		if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
			a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() || cgnat.Contains(a) {
			return false
		}
	}
	return true
}

// truncate keeps ~75% head and ~25% tail cut on line boundaries, with
// upstream's footer. The full text goes to CacheDir when configured.
func (w *Web) truncate(content, pageURL string, limit int) string {
	r := []rune(content)
	if len(r) <= limit {
		return content
	}
	headBudget := int(float64(limit) * 0.75)
	tailBudget := limit - headBudget
	head, tail := string(r[:headBudget]), string(r[len(r)-tailBudget:])
	if nl := strings.LastIndex(head, "\n"); nl >= 0 && float64(utf8.RuneCountInString(head[:nl])) > float64(headBudget)*0.5 {
		head = head[:nl]
	}
	if nl := strings.Index(tail, "\n"); nl >= 0 && float64(utf8.RuneCountInString(tail[:nl])) < float64(tailBudget)*0.5 {
		tail = tail[nl+1:]
	}
	footer := []string{"", strings.Repeat("─", 8) + " [TRUNCATED] " + strings.Repeat("─", 8),
		fmt.Sprintf("Showing %s chars (head) + %s chars (tail) of %s total clean characters.",
			comma(utf8.RuneCountInString(head)), comma(utf8.RuneCountInString(tail)), comma(len(r)))}
	if stored := w.store(pageURL, content); stored != "" {
		footer = append(footer, "Full text saved to: "+stored,
			fmt.Sprintf(`To read the omitted middle: read_file path="%s" offset=%d limit=200  (the file is the complete page; raise/lower offset to page through it).`,
				stored, strings.Count(head, "\n")+2))
	} else {
		footer = append(footer, "Full text could not be stored; re-run web_extract on a more specific URL or open it in the browser for the complete page.")
	}
	footer = append(footer, strings.Repeat("─", 29))
	return head + "\n\n[... middle omitted — see footer ...]\n\n" + tail + "\n" + strings.Join(footer, "\n")
}

func (w *Web) store(pageURL, content string) string {
	if w.o.CacheDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(pageURL))
	p := filepath.Join(w.o.CacheDir, hex.EncodeToString(sum[:8])+".txt")
	if err := os.MkdirAll(w.o.CacheDir, 0o700); err != nil {
		w.o.Logger.Warn("web cache", "err", err)
		return ""
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		w.o.Logger.Warn("web cache", "err", err)
		return ""
	}
	return p
}

// comma formats like Python's "{:,}".
func comma(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
