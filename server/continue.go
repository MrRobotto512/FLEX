package server

import (
	"fmt"
	"net/http"
	"sort"
	"time"
)

// ContinueItem is one entry of the "Continue Watching" row: a movie that was
// stopped part way, or the episode to watch next in a show (the one stopped
// part way, else the first unwatched one after the last played).
type ContinueItem struct {
	Kind     string    `json:"kind"` // "movie" | "episode"; ID is what /movies|/episodes/{id}/play takes
	ID       int64     `json:"id"`
	Title    string    `json:"title"`              // movie or show title
	Subtitle string    `json:"subtitle,omitempty"` // "S02E05 · Title" for episodes
	Poster   string    `json:"poster,omitempty"`   // the movie's or the show's
	TMDBID   int64     `json:"tmdb_id,omitempty"`
	PosterOf string    `json:"poster_of"` // "movie" | "show": whose id PosterID is
	PosterID int64     `json:"poster_id"`
	ResumeS  int       `json:"resume_s"` // 0: the next episode, not started yet
	RuntimeS int       `json:"runtime_s,omitempty"`
	ShowID   int64     `json:"show_id,omitempty"`
	Played   time.Time `json:"played"`
}

// ContinueWatching returns up to limit items, most recently played first.
func (d *DB) ContinueWatching(limit int) ([]ContinueItem, error) {
	out := []ContinueItem{}

	rows, err := d.Query(`SELECT item_id, updated_at FROM progress
		WHERE kind = 'movie' AND watched = 0 AND position_s > 0 ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	type played struct {
		id int64
		at time.Time
	}
	var movies []played
	for rows.Next() {
		var p played
		var at string
		if err := rows.Scan(&p.id, &at); err != nil {
			rows.Close()
			return nil, err
		}
		p.at, _ = time.Parse(time.RFC3339, at)
		movies = append(movies, p)
	}
	rows.Close()
	for _, p := range movies {
		m, err := d.GetMovie(p.id)
		if err != nil {
			continue // removed since
		}
		out = append(out, ContinueItem{Kind: "movie", ID: m.ID, Title: m.Title, Poster: m.Poster, TMDBID: m.TMDBID,
			PosterOf: "movie", PosterID: m.ID, ResumeS: m.ResumeS, RuntimeS: m.RuntimeS, Played: p.at})
	}

	// the most recently played episode of each show
	rows, err = d.Query(`SELECT e.show_id, p.item_id, p.updated_at FROM progress p
		JOIN episodes e ON p.kind = 'episode' AND e.id = p.item_id ORDER BY p.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	type last struct {
		show, ep int64
		at       time.Time
	}
	var shows []last
	seen := map[int64]bool{}
	for rows.Next() {
		var l last
		var at string
		if err := rows.Scan(&l.show, &l.ep, &at); err != nil {
			rows.Close()
			return nil, err
		}
		if seen[l.show] || len(shows) >= limit {
			continue
		}
		seen[l.show] = true
		l.at, _ = time.Parse(time.RFC3339, at)
		shows = append(shows, l)
	}
	rows.Close()
	for _, l := range shows {
		s, err := d.GetShow(l.show)
		if err != nil {
			continue
		}
		if ep := nextEpisode(s, l.ep); ep != nil {
			out = append(out, ContinueItem{Kind: "episode", ID: ep.ID, Title: s.Title,
				Subtitle: fmt.Sprintf("S%02dE%02d · %s", ep.Season, ep.Episode, ep.Title),
				Poster:   s.Poster, TMDBID: s.TMDBID, PosterOf: "show", PosterID: s.ID,
				ResumeS: ep.ResumeS, RuntimeS: ep.RuntimeS, ShowID: s.ID, Played: l.at})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Played.After(out[j].Played) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// nextEpisode is the episode to offer after lastID was played: lastID itself
// while it is part way, else the first unwatched episode after it. nil when
// the show is finished.
func nextEpisode(s *Show, lastID int64) *Episode {
	var eps []*Episode
	for i := range s.SeasonList {
		for j := range s.SeasonList[i].Episodes {
			eps = append(eps, &s.SeasonList[i].Episodes[j])
		}
	}
	for i, ep := range eps {
		if ep.ID != lastID {
			continue
		}
		if !ep.Watched && ep.ResumeS > 0 {
			return ep
		}
		for _, next := range eps[i+1:] {
			if !next.Watched {
				return next
			}
		}
		return nil
	}
	return nil
}

func (s *Server) handleContinue(w http.ResponseWriter, r *http.Request) {
	items, err := s.db.ContinueWatching(intOr(r.URL.Query().Get("limit"), 20))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}
