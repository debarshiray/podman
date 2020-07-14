package compat

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containers/libpod/v2/libpod"
	"github.com/containers/libpod/v2/libpod/logs"
	"github.com/containers/libpod/v2/pkg/api/handlers/utils"
	"github.com/containers/libpod/v2/pkg/util"
	"github.com/gorilla/schema"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

func LogsFromContainer(w http.ResponseWriter, r *http.Request) {
	decoder := r.Context().Value("decoder").(*schema.Decoder)
	runtime := r.Context().Value("runtime").(*libpod.Runtime)

	query := struct {
		Follow     bool   `schema:"follow"`
		Stdout     bool   `schema:"stdout"`
		Stderr     bool   `schema:"stderr"`
		Since      string `schema:"since"`
		Until      string `schema:"until"`
		Timestamps bool   `schema:"timestamps"`
		Tail       string `schema:"tail"`
	}{
		Tail: "all",
	}
	if err := decoder.Decode(&query, r.URL.Query()); err != nil {
		utils.Error(w, "Something went wrong.", http.StatusBadRequest, errors.Wrapf(err, "Failed to parse parameters for %s", r.URL.String()))
		return
	}

	if !(query.Stdout || query.Stderr) {
		msg := fmt.Sprintf("%s: you must choose at least one stream", http.StatusText(http.StatusBadRequest))
		utils.Error(w, msg, http.StatusBadRequest, errors.Errorf("%s for %s", msg, r.URL.String()))
		return
	}

	name := utils.GetName(r)
	ctnr, err := runtime.LookupContainer(name)
	if err != nil {
		utils.ContainerNotFound(w, name, err)
		return
	}

	var tail int64 = -1
	if query.Tail != "all" {
		tail, err = strconv.ParseInt(query.Tail, 0, 64)
		if err != nil {
			utils.BadRequest(w, "tail", query.Tail, err)
			return
		}
	}

	var since time.Time
	if _, found := r.URL.Query()["since"]; found {
		since, err = util.ParseInputTime(query.Since)
		if err != nil {
			utils.BadRequest(w, "since", query.Since, err)
			return
		}
	}

	var until time.Time
	if _, found := r.URL.Query()["until"]; found {
		// FIXME: until != since but the logs backend does not yet support until.
		since, err = util.ParseInputTime(query.Until)
		if err != nil {
			utils.BadRequest(w, "until", query.Until, err)
			return
		}
	}

	options := &logs.LogOptions{
		Details:    true,
		Follow:     query.Follow,
		Since:      since,
		Tail:       tail,
		Timestamps: query.Timestamps,
	}

	var wg sync.WaitGroup
	options.WaitGroup = &wg

	logChannel := make(chan *logs.LogLine, tail+1)
	if err := runtime.Log(r.Context(), []*libpod.Container{ctnr}, options, logChannel); err != nil {
		utils.InternalServerError(w, errors.Wrapf(err, "Failed to obtain logs for Container '%s'", name))
		return
	}
	go func() {
		wg.Wait()
		close(logChannel)
	}()

	w.WriteHeader(http.StatusOK)

	var frame strings.Builder
	header := make([]byte, 8)
	for line := range logChannel {
		if _, found := r.URL.Query()["until"]; found {
			if line.Time.After(until) {
				break
			}
		}

		// Reset buffer we're ready to loop again
		frame.Reset()
		switch line.Device {
		case "stdout":
			if !query.Stdout {
				continue
			}
			header[0] = 1
		case "stderr":
			if !query.Stderr {
				continue
			}
			header[0] = 2
		default:
			// Logging and moving on is the best we can do here. We may have already sent
			// a Status and Content-Type to client therefore we can no longer report an error.
			log.Infof("unknown Device type '%s' in log file from Container %s", line.Device, ctnr.ID())
			continue
		}

		if query.Timestamps {
			frame.WriteString(line.Time.Format(time.RFC3339))
			frame.WriteString(" ")
		}
		frame.WriteString(line.Msg)

		binary.BigEndian.PutUint32(header[4:], uint32(frame.Len()))
		if _, err := w.Write(header[0:8]); err != nil {
			log.Errorf("unable to write log output header: %q", err)
		}
		if _, err := io.WriteString(w, frame.String()); err != nil {
			log.Errorf("unable to write frame string: %q", err)
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}