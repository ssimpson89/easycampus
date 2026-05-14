package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// createBookmark appends a bookmark constructed from the typed request.
// Empty Name falls back to "Unnamed Bookmark"; FPS defaults to 30 to
// match the legacy UI behavior when the client omits the field.
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

	bookmark := Bookmark{
		ID:        "bm_" + padInt(len(bookmarks)+1, 3),
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

	next := bookmarks[:0]
	removed := false
	for _, bm := range bookmarks {
		if bm.ID == bookmarkID {
			removed = true
			continue
		}
		next = append(next, bm)
	}

	if !removed {
		return false, nil
	}

	if err := writeBookmarks(settings, eventID, next); err != nil {
		return false, err
	}
	return true, nil
}
