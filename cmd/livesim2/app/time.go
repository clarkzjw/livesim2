package app

import (
	"net/http"
	"strconv"
	"time"
)

func (s *Server) UTCTimeHandlerFunc(w http.ResponseWriter, r *http.Request) {
	nowMS := int(time.Now().Unix())
	w.Write([]byte(strconv.Itoa(nowMS)))
}

func (s *Server) UTCISOTimeHandlerFunc(w http.ResponseWriter, r *http.Request) {
	nowMS := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	w.Write([]byte(nowMS))
}
