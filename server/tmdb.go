package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	tmdbAPI   = "https://api.themoviedb.org/3"
	tmdbImage = "https://image.tmdb.org/t/p"
)

// TMDB looks up movie metadata and caches posters on disk.
type TMDB struct {
	key       string
	posterDir string
	http      *http.Client
	log       *slog.Logger
}

func NewTMDB(key, posterDir string, log *slog.Logger) *TMDB {
	return &TMDB{
		key:       key,
		posterDir: posterDir,
		http:      &http.Client{Timeout: 20 * time.Second},
		log:       log,
	}
}

type tmdbMovie struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	ReleaseDate  string `json:"release_date"`
	Overview     string `json:"overview"`
	PosterPath   string `json:"poster_path"`
	BackdropPath string `json:"backdrop_path"`
	Runtime      int    `json:"runtime"`
}

var ErrNoMatch = errors.New("tmdb: no match")

// Match searches by title/year and fetches full details plus images.
func (t *TMDB) Match(ctx context.Context, title string, year int) (Metadata, error) {
	hit, err := t.search(ctx, title, year)
	if err != nil && year != 0 {
		hit, err = t.search(ctx, title, 0) // year in the filename may be wrong
	}
	if err != nil {
		return Metadata{}, err
	}
	return t.Fetch(ctx, hit.ID)
}

// Fetch pulls details for a known TMDB id (used by the manual fix-match endpoint).
func (t *TMDB) Fetch(ctx context.Context, id int64) (Metadata, error) {
	var m tmdbMovie
	if err := t.get(ctx, fmt.Sprintf("/movie/%d", id), nil, &m); err != nil {
		return Metadata{}, err
	}
	md := Metadata{
		TMDBID:   m.ID,
		Title:    m.Title,
		Overview: m.Overview,
		RuntimeS: m.Runtime * 60,
	}
	if len(m.ReleaseDate) >= 4 {
		md.Year, _ = strconv.Atoi(m.ReleaseDate[:4])
	}
	if m.PosterPath != "" {
		md.PosterPath = t.image(ctx, "w500", m.PosterPath, fmt.Sprintf("%d_poster.jpg", m.ID))
	}
	if m.BackdropPath != "" {
		md.BackdropPath = t.image(ctx, "w1280", m.BackdropPath, fmt.Sprintf("%d_backdrop.jpg", m.ID))
	}
	return md, nil
}

func (t *TMDB) search(ctx context.Context, title string, year int) (*tmdbMovie, error) {
	q := url.Values{"query": {title}, "include_adult": {"false"}}
	if year != 0 {
		q.Set("year", strconv.Itoa(year))
	}
	var res struct {
		Results []tmdbMovie `json:"results"`
	}
	if err := t.get(ctx, "/search/movie", q, &res); err != nil {
		return nil, err
	}
	if len(res.Results) == 0 {
		return nil, ErrNoMatch
	}
	return &res.Results[0], nil
}

func (t *TMDB) get(ctx context.Context, path string, q url.Values, into any) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("api_key", t.key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tmdbAPI+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tmdb %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// image downloads one TMDB image into posterDir and returns the local path,
// or "" if the download failed. Already-cached files are reused.
func (t *TMDB) image(ctx context.Context, size, remote, name string) string {
	dest := filepath.Join(t.posterDir, name)
	if _, err := os.Stat(dest); err == nil {
		return dest
	}
	if err := os.MkdirAll(t.posterDir, 0o755); err != nil {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tmdbImage+"/"+size+remote, nil)
	if err != nil {
		return ""
	}
	resp, err := t.http.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		t.log.Warn("tmdb: image download failed", "path", remote, "err", err)
		return ""
	}
	defer resp.Body.Close()
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return ""
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return ""
	}
	f.Close()
	if err := os.Rename(tmp, dest); err != nil {
		return ""
	}
	return dest
}
