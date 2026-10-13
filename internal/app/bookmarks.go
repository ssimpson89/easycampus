package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"easycampus/internal/atomicfile"
)

func readBookmarks(settings AppSettings, eventID string) ([]Bookmark, error) {
	bookmarksPath := filepath.Join(settings.EventsDir, eventID, "bookmarks.json")
	data, err := os.ReadFile(bookmarksPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Bookmark{}, nil
		}
		return nil, err
	}

	var bookmarks []Bookmark
	if err := json.Unmarshal(data, &bookmarks); err != nil {
		return nil, err
	}
	return bookmarks, nil
}

func writeBookmarks(settings AppSettings, eventID string, bookmarks []Bookmark) error {
	bookmarksPath := filepath.Join(settings.EventsDir, eventID, "bookmarks.json")
	data, err := json.MarshalIndent(bookmarks, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write — concurrent POST + DELETE on the same event's
	// bookmark list would otherwise race and yield a truncated file
	// that subsequent reads silently parse as zero bookmarks.
	return atomicfile.Write(bookmarksPath, data, 0644)
}

var (
	errInvalidBookmarkID = errors.New("invalid bookmark id")
	errBookmarkExists    = errors.New("bookmark id already exists")
)

// nextBookmarkID returns "bm_NNN" one past the highest numeric suffix in
// use. Unlike len+1 this never re-issues an ID after a delete.
func nextBookmarkID(bookmarks []Bookmark) string {
	maxN := 0
	for _, bm := range bookmarks {
		if rest, ok := strings.CutPrefix(bm.ID, "bm_"); ok {
			if n, err := strconv.Atoi(rest); err == nil && n > maxN {
				maxN = n
			}
		}
	}
	return "bm_" + padInt(maxN+1, 3)
}

// createBookmark appends a bookmark constructed from the typed request.
// Empty Name falls back to "Unnamed Bookmark"; FPS defaults to 30 to
// match the legacy UI behavior when the client omits the field.
//
// Callers must serialize calls per event (Server.bookmarkLocks); this
// is a read-modify-write of bookmarks.json.
func createBookmark(settings AppSettings, eventID string, req BookmarkCreateRequest) (Bookmark, error) {
	bookmarks, err := readBookmarks(settings, eventID)
	if err != nil {
		return Bookmark{}, err
	}

	name := req.Name
	if name == "" {
		name = "Unnamed Bookmark"
	}
	fps := req.FPS
	if fps == 0 {
		fps = 30.0
	}

	id := nextBookmarkID(bookmarks)
	if req.ID != "" {
		if !validID(req.ID) {
			return Bookmark{}, errInvalidBookmarkID
		}
		for _, bm := range bookmarks {
			if bm.ID == req.ID {
				return Bookmark{}, errBookmarkExists
			}
		}
		id = req.ID
	}

	bookmark := Bookmark{
		ID:        id,
		Name:      name,
		Frame:     req.Frame,
		FPS:       fps,
		TimePos:   req.TimeSecs,
		CreatedAt: time.Now().Format(time.RFC3339),
		Source:    "encoder",
		Color:     "blue",
		Notes:     req.Notes,
	}

	bookmarks = append(bookmarks, bookmark)
	if err := writeBookmarks(settings, eventID, bookmarks); err != nil {
		return Bookmark{}, err
	}

	return bookmark, nil
}

// updateBookmark applies a typed BookmarkUpdateRequest to the named
// bookmark. Only fields with non-nil pointers are written; an empty
// string is a valid value to write (e.g. clearing a note).
func updateBookmark(settings AppSettings, eventID, bookmarkID string, req BookmarkUpdateRequest) (Bookmark, error) {
	bookmarks, err := readBookmarks(settings, eventID)
	if err != nil {
		return Bookmark{}, err
	}

	for i := range bookmarks {
		if bookmarks[i].ID == bookmarkID {
			if req.Name != nil {
				bookmarks[i].Name = *req.Name
			}
			if req.Color != nil {
				bookmarks[i].Color = *req.Color
			}
			if req.Notes != nil {
				bookmarks[i].Notes = *req.Notes
			}
			if err := writeBookmarks(settings, eventID, bookmarks); err != nil {
				return Bookmark{}, err
			}
			return bookmarks[i], nil
		}
	}

	return Bookmark{}, errors.New("bookmark not found")
}

func deleteBookmark(settings AppSettings, eventID, bookmarkID string) (bool, error) {
	bookmarks, err := readBookmarks(settings, eventID)
	if err != nil {
		return false, err
	}

	// Remove only the first match: legacy files written by the old
	// len+1 allocator can contain duplicate IDs, and a delete must not
	// silently take out an unrelated bookmark that shares the ID.
	idx := -1
	for i, bm := range bookmarks {
		if bm.ID == bookmarkID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}
	next := append(bookmarks[:idx:idx], bookmarks[idx+1:]...)

	if err := writeBookmarks(settings, eventID, next); err != nil {
		return false, err
	}
	return true, nil
}
