package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearch_Movie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [
				{"id": 27205, "title": "Inception", "release_date": "2010-07-16",
				 "overview": "A thief in dreams.", "vote_average": 8.3,
				 "poster_path": "/poster.jpg", "backdrop_path": "/back.jpg"}
			]
		}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "k", BaseURL: srv.URL, HTTP: srv.Client()}
	m, err := c.Search(context.Background(), "movie", "Inception", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if m == nil {
		t.Fatal("nil meta")
	}
	if m.TMDBID != 27205 || m.Title != "Inception" || m.Year != 2010 {
		t.Errorf("got %+v", m)
	}
	if m.Rating != 8.3 {
		t.Errorf("rating = %v", m.Rating)
	}
	if got := ImageURL(m.PosterPath, "w500"); got != "https://image.tmdb.org/t/p/w500/poster.jpg" {
		t.Errorf("poster url = %q", got)
	}
}

func TestSearch_NoKey(t *testing.T) {
	c := New("")
	_, err := c.Search(context.Background(), "movie", "x", 0)
	if err == nil {
		t.Error("expected error when api key empty")
	}
}
