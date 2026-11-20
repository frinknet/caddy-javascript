package handler

import (
	"errors"
	"net/http"
)

// streamWriter is the handler side of a streaming response. The VM hands it
// one chunk at a time, and every chunk is flushed so SSE clients see it
// immediately.
type streamWriter struct {
	w           http.ResponseWriter
	rc          *http.ResponseController
	wroteHeader bool
}

func (s *streamWriter) WriteHeader(status int, headers map[string]string) error {
	if s.wroteHeader {
		return errors.New("javascript: response already started")
	}
	s.wroteHeader = true
	for key, value := range headers {
		s.w.Header().Set(key, value)
	}
	s.w.WriteHeader(status)
	return nil
}

func (s *streamWriter) Write(p []byte) error {
	if !s.wroteHeader {
		if err := s.WriteHeader(http.StatusOK, nil); err != nil {
			return err
		}
	}
	_, err := s.w.Write(p)
	return err
}

func (s *streamWriter) Flush() error {
	if err := s.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}
