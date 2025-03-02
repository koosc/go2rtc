package mp4

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/api/ws"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/rs/zerolog"
)

func Init() {
	log = app.GetLogger("mp4")

	ws.HandleFunc("mse", handlerWSMSE)
	ws.HandleFunc("mp4", handlerWSMP4)

	api.HandleFunc("api/frame.mp4", handlerKeyframe)
	api.HandleFunc("api/stream.mp4", handlerMP4)
}

var log zerolog.Logger

func handlerKeyframe(w http.ResponseWriter, r *http.Request) {
	// Chrome 105 does two requests: without Range and with `Range: bytes=0-`
	ua := r.UserAgent()
	if strings.Contains(ua, " Chrome/") {
		if r.Header.Values("Range") == nil {
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	query := r.URL.Query()
	src := query.Get("src")
	stream := streams.Get(src)
	if stream == nil {
		http.Error(w, api.StreamNotFound, http.StatusNotFound)
		return
	}

	exit := make(chan []byte, 1)

	cons := &mp4.Segment{OnlyKeyframe: true}
	cons.Listen(func(msg any) {
		if data, ok := msg.([]byte); ok && exit != nil {
			select {
			case exit <- data:
			default:
			}
			exit = nil
		}
	})

	if err := stream.AddConsumer(cons); err != nil {
		log.Error().Err(err).Caller().Send()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := <-exit

	stream.RemoveConsumer(cons)

	// Apple Safari won't show frame without length
	header := w.Header()
	header.Set("Content-Length", strconv.Itoa(len(data)))
	header.Set("Content-Type", cons.MimeType)

	if filename := query.Get("filename"); filename != "" {
		header.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	}

	if _, err := w.Write(data); err != nil {
		log.Error().Err(err).Caller().Send()
	}
}

func handlerMP4(w http.ResponseWriter, r *http.Request) {
	log.Trace().Msgf("[mp4] %s %+v", r.Method, r.Header)

	query := r.URL.Query()

	ua := r.UserAgent()
	if strings.Contains(ua, " Safari/") && !strings.Contains(ua, " Chrome/") && !query.Has("duration") {
		// auto redirect to HLS/fMP4 format, because Safari not support MP4 stream
		url := "stream.m3u8?" + r.URL.RawQuery
		if !query.Has("mp4") {
			url += "&mp4"
		}

		http.Redirect(w, r, url, http.StatusMovedPermanently)
		return
	}

	src := query.Get("src")
	stream := streams.Get(src)
	if stream == nil {
		http.Error(w, api.StreamNotFound, http.StatusNotFound)
		return
	}

	exit := make(chan error, 1) // Add buffer to prevent blocking

	cons := &mp4.Consumer{
		Desc:       "MP4/HTTP",
		RemoteAddr: tcp.RemoteAddr(r),
		UserAgent:  r.UserAgent(),
		Medias:     mp4.ParseQuery(r.URL.Query()),
	}

	cons.Listen(func(msg any) {
		if exit == nil {
			return
		}
		if data, ok := msg.([]byte); ok {
			if _, err := w.Write(data); err != nil {
				select {
				case exit <- err:
				default:
				}
				exit = nil
			}
		}
	})

	if err := stream.AddConsumer(cons); err != nil {
		log.Error().Err(err).Caller().Send()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	defer stream.RemoveConsumer(cons)

	data, err := cons.Init()
	if err != nil {
		log.Error().Err(err).Caller().Send()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	header := w.Header()
	header.Set("Content-Type", cons.MimeType())

	if filename := query.Get("filename"); filename != "" {
		header.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	}

	if rotate := query.Get("rotate"); rotate != "" {
		mp4.PatchVideoRotate(data, core.Atoi(rotate))
	}

	if scale := query.Get("scale"); scale != "" {
		if sx, sy, ok := strings.Cut(scale, ":"); ok {
			mp4.PatchVideoScale(data, core.Atoi(sx), core.Atoi(sy))
		}
	}

	if _, err = w.Write(data); err != nil {
		log.Error().Err(err).Caller().Send()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cons.Start()

	var duration *time.Timer
	if s := query.Get("duration"); s != "" {
		if i, _ := strconv.Atoi(s); i > 0 {
			duration = time.AfterFunc(time.Second*time.Duration(i), func() {
				if exit != nil {
					select {
					case exit <- nil:
					default:
					}
					exit = nil
				}
			})
		}
	}

	err = <-exit
	exit = nil

	log.Trace().Err(err).Caller().Send()

	if duration != nil {
		duration.Stop()
	}
}
