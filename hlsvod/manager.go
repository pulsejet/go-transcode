package hlsvod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// how long can it take for transcode to be ready
const readyTimeout = 24 * time.Second

type ManagerCtx struct {
	logger zerolog.Logger
	mu     sync.Mutex
	config Config

	segmentDuration float64
	segmentSuffix   string

	ready         bool
	onReadyChange chan struct{}

	events struct {
		onStart  func()
		onCmdLog func(message string)
		onStop   func(err error)
	}

	metadata      *ProbeMediaData
	playlist      string       // m3u8 playlist string
	segments      map[int]bool // map of segments and their availability
	segmentsTimes []float64    // list of breakpoints for segments

	shutdown chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

func New(config Config) *ManagerCtx {
	ctx, cancel := context.WithCancel(context.Background())
	return &ManagerCtx{
		logger: log.With().Str("module", "hlsvod").Str("submodule", "manager").Logger(),
		config: config,

		segmentDuration: 4.75,
		segmentSuffix:   "-%05d.ts",

		ctx:    ctx,
		cancel: cancel,
	}
}

// fetch metadata using ffprobe
func (m *ManagerCtx) fetchMetadata() error {
	log.Info().Msg("fetching metadata")

	// start ffprobe to get metadata about current media
	metadata, err := ProbeMedia(m.ctx, m.config.FFprobeBinary, m.config.MediaPath)
	if err != nil {
		return fmt.Errorf("unable probe media for metadata: %v", err)
	}

	// if media has video, use keyframes as reference for segments
	if metadata.Video != nil && metadata.Video.PktPtsTime == nil {
		// start ffprobe to get keyframes from video
		videoData, err := ProbeVideo(m.ctx, m.config.FFprobeBinary, m.config.MediaPath)
		if err != nil {
			return fmt.Errorf("unable probe video for keyframes: %v", err)
		}
		metadata.Video.PktPtsTime = videoData.PktPtsTime
	}

	log.Info().Interface("metadata", metadata).Msg("fetched metadata")
	m.metadata = metadata
	return nil
}

// load metadata from cache or fetch them and cache
func (m *ManagerCtx) loadMetadata() error {
	log.Info().Msg("loading metadata")

	// bypass cache if not enabled
	if !m.config.Cache {
		return m.fetchMetadata()
	}

	// try to get cached data
	data, err := m.getCacheData()
	if err == nil {
		// unmarshall cache data
		err := json.Unmarshal(data, &m.metadata)
		if err == nil {
			return nil
		}

		log.Err(err).Msg("cache unmarhalling returned error, replacing")
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Err(err).Msg("cache hit returned error, replacing")
	}

	// fetch fresh metadata from a file
	if err := m.fetchMetadata(); err != nil {
		return err
	}

	// marshall new metadata to bytes
	data, err = json.Marshal(m.metadata)
	if err != nil {
		return err
	}

	if m.config.CacheDir != "" {
		return m.saveGlobalCacheData(data)
	}

	return m.saveLocalCacheData(data)
}

func (m *ManagerCtx) getSegmentName(index int) string {
	return m.config.SegmentPrefix + fmt.Sprintf(m.segmentSuffix, index)
}

func (m *ManagerCtx) getPlaylist() string {
	// playlist prefix
	playlist := []string{
		"#EXTM3U",
		"#EXT-X-VERSION:4",
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-MEDIA-SEQUENCE:0",
		fmt.Sprintf("#EXT-X-TARGETDURATION:%.2f", m.segmentDuration),
	}

	// playlist segments
	for i := 1; i < len(m.segmentsTimes); i++ {
		playlist = append(playlist,
			fmt.Sprintf("#EXTINF:%.3f, no desc", m.segmentsTimes[i]-m.segmentsTimes[i-1]),
			m.getSegmentName(i),
		)
	}

	// playlist suffix
	playlist = append(playlist,
		"#EXT-X-ENDLIST",
	)

	// join with newlines
	return strings.Join(playlist, "\n")
}

func (m *ManagerCtx) initialize() {
	// TODO: Generate segment times from keyframes.
	m.segmentsTimes = m.metadata.Video.PktPtsTime

	// generate playlist
	m.playlist = m.getPlaylist()

	// prepare transcode matrix from segment times
	m.segments = map[int]bool{}
	for i := 1; i < len(m.segmentsTimes); i++ {
		m.segments[i] = false
	}

	log.Info().Interface("metadata", m.metadata).Msg("loaded metadata")
}

func (m *ManagerCtx) Start() (err error) {
	if m.ready {
		return fmt.Errorf("already running")
	}

	m.mu.Lock()
	// initialize signaling channels
	m.shutdown = make(chan struct{})
	m.ready = false
	m.onReadyChange = make(chan struct{})
	m.mu.Unlock()

	// initialize transcoder asynchronously
	go func() {
		if err := m.loadMetadata(); err != nil {
			log.Printf("%v\n", err)
			return
		}

		// initialization based on metadata
		m.initialize()

		m.mu.Lock()
		// set video to ready state
		m.ready = true
		close(m.onReadyChange)
		m.mu.Unlock()
	}()

	// TODO: Cleanup process.

	return nil
}

func (m *ManagerCtx) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// stop all transcoding processes
	// remove all transcoded segments

	m.cancel()
	close(m.shutdown)

	m.ready = false
}

func (m *ManagerCtx) Cleanup() {
	// check what segments are really needed
	// stop transcoding processes that are not needed anymore
}

func (m *ManagerCtx) ServePlaylist(w http.ResponseWriter, r *http.Request) {
	// ensure that transcode started
	if !m.ready {
		select {
		// waiting for transcode to be ready
		case <-m.onReadyChange:
			// check if it started succesfully
			if !m.ready {
				m.logger.Warn().Msgf("playlist load failed")
				http.Error(w, "504 playlist not available", http.StatusInternalServerError)
				return
			}
		// when transcode stops before getting ready
		case <-m.shutdown:
			m.logger.Warn().Msg("playlist load failed because of shutdown")
			http.Error(w, "500 playlist not available", http.StatusInternalServerError)
			return
		case <-time.After(readyTimeout):
			m.logger.Warn().Msg("playlist load timeouted")
			http.Error(w, "504 playlist timeout", http.StatusGatewayTimeout)
			return
		}
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write([]byte(m.playlist))
}

func (m *ManagerCtx) ServeMedia(w http.ResponseWriter, r *http.Request) {
	// TODO: get index from URL
	index := 0

	available, ok := m.segments[index]
	if !ok {
		m.logger.Warn().Int("index", index).Msg("media index not found")
		http.Error(w, "404 index not found", http.StatusNotFound)
		return
	}

	// check if media is already transcoded
	if !available {
		m.logger.Warn().Int("index", index).Msg("media needs to be transcoded")
		// TODO:
		//	- if not, check if probe data exists
		//	-	- if not, check if probe is not running
		//	-	-	- if not, start it
		//	-	- wait for it to finish
		//	- start transcoding from this segment
		//	- wait for this segment to finish
	}

	segmentName := m.getSegmentName(index)
	segmentPath := path.Join(m.config.TranscodeDir, segmentName)

	if _, err := os.Stat(segmentPath); os.IsNotExist(err) {
		m.logger.Warn().Int("index", index).Str("path", segmentPath).Msg("media file not found")
		http.Error(w, "404 media not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")

	// return existing segment
	http.ServeFile(w, r, segmentPath)
}

func (m *ManagerCtx) OnStart(event func()) {
	m.events.onStart = event
}

func (m *ManagerCtx) OnCmdLog(event func(message string)) {
	m.events.onCmdLog = event
}

func (m *ManagerCtx) OnStop(event func(err error)) {
	m.events.onStop = event
}
