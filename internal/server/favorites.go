package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Favorite is a channel saved for one-tap tuning. It names a virtual channel
// from the lineup or a physical RF channel.
type Favorite struct {
	VChannel string `json:"vchannel,omitempty"`
	RF       int    `json:"rf,omitempty"`
	Label    string `json:"label,omitempty"`
}

const maxFavorites = 50

// favorites keeps every device's favorites in one JSON file, shared by all
// viewers. Writes replace the file atomically.
type favorites struct {
	path string

	mu   sync.Mutex
	byID map[string][]Favorite // by device id
}

func loadFavorites(dir string) (*favorites, error) {
	f := &favorites{byID: map[string][]Favorite{}}
	if dir == "" {
		return f, nil // in memory only
	}
	f.path = filepath.Join(dir, "favorites.json")
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &f.byID); err != nil {
		return nil, fmt.Errorf("%s: %w", f.path, err)
	}
	return f, nil
}

func (f *favorites) get(id string) []Favorite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Favorite{}, f.byID[id]...)
}

func (f *favorites) set(id string, favs []Favorite) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	next := make(map[string][]Favorite, len(f.byID)+1)
	for k, v := range f.byID {
		next[k] = v
	}
	if len(favs) == 0 {
		delete(next, id)
	} else {
		next[id] = favs
	}

	if f.path != "" {
		data, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return err
		}
		tmp := f.path + ".tmp"
		if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, f.path); err != nil {
			return err
		}
	}
	f.byID = next
	return nil
}

func validFavorites(favs []Favorite) error {
	if len(favs) > maxFavorites {
		return fmt.Errorf("at most %d favorites", maxFavorites)
	}
	seen := map[string]bool{}
	for i := range favs {
		fav := &favs[i]
		fav.VChannel = strings.TrimSpace(fav.VChannel)
		fav.Label = strings.TrimSpace(fav.Label)
		switch {
		case fav.VChannel != "" && fav.RF == 0:
			if len(fav.VChannel) > 16 {
				return errors.New("vchannel is too long")
			}
		case fav.RF != 0 && fav.VChannel == "":
			if fav.RF < 2 || fav.RF > 69 {
				return errors.New("RF channel must be 2–69")
			}
		default:
			return errors.New("each favorite needs one of vchannel or rf")
		}
		if len(fav.Label) > 40 {
			return errors.New("label is too long")
		}
		key := fmt.Sprintf("%s|%d", fav.VChannel, fav.RF)
		if seen[key] {
			return errors.New("duplicate favorite")
		}
		seen[key] = true
	}
	return nil
}

func (s *Server) handleGetFavorites(w http.ResponseWriter, r *http.Request) {
	d := s.device(w, r)
	if d == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.favs.get(d.id))
}

// handlePutFavorites replaces the device's favorites with the list in the
// body, in that order.
func (s *Server) handlePutFavorites(w http.ResponseWriter, r *http.Request) {
	d := s.device(w, r)
	if d == nil {
		return
	}
	var favs []Favorite
	if !readJSON(w, r, &favs) {
		return
	}
	if err := validFavorites(favs); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.favs.set(d.id, favs); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.favs.get(d.id))
}
