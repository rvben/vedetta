package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Cameras       []CameraConfig         `yaml:"cameras"`
	Detect        DetectConfig           `yaml:"detect"`
	Recording     RecordingConfig        `yaml:"recording"`
	Events        EventConfig            `yaml:"events"`
	Storage       StorageConfig          `yaml:"storage"`
	MQTT          MQTTConfig             `yaml:"mqtt"`
	API           APIConfig              `yaml:"api"`
	RTSPServer    RTSPServerConfig       `yaml:"rtsp_server"`
	Auth          AuthConfig             `yaml:"auth"`
	Updates       UpdateConfig           `yaml:"updates"`
	Codecs        CodecsConfig           `yaml:"codecs"`
	Notifications NotificationsConfig    `yaml:"notifications"`
	WebRTC        WebRTCConfig           `yaml:"webrtc"`
	Doorbell      DoorbellDefaultsConfig `yaml:"doorbell"`
	Tracing       TracingConfig          `yaml:"tracing"`
	Logging       LoggingConfig          `yaml:"logging"`
	Runtime       RuntimeConfig          `yaml:"runtime"`
}

// RuntimeConfig holds process-level safety limits.
type RuntimeConfig struct {
	// MemoryGuard enables the graceful memory-pressure self-restart. When the
	// process's Go-runtime memory footprint stays above the limit for a
	// sustained window, vedetta shuts down cleanly and lets the supervisor
	// restart it, instead of being SIGKILLed by the OS OOM killer (macOS jetsam,
	// Linux oom-killer) - which abandons in-flight recordings. Enabled by
	// default.
	MemoryGuard bool `yaml:"memory_guard"`

	// MemoryLimitMB is the footprint ceiling in megabytes. 0 (the default) means
	// auto: 60% of total system RAM. An explicit positive value overrides the
	// auto calculation. Ignored when MemoryGuard is false.
	MemoryLimitMB int `yaml:"memory_limit_mb"`
}

// NotificationsConfig controls web push notification delivery.
type NotificationsConfig struct {
	// VAPIDSubscriber is the contact identifier embedded in every VAPID
	// JWT as the `sub` claim, per RFC 8292. The push service operator
	// (Apple, Google, Mozilla) uses it to reach the application server
	// operator in case of abuse.
	//
	// Must be EITHER:
	//   - a raw email address: "admin@example.com" (webpush-go will
	//     prepend "mailto:" itself; do NOT include the scheme)
	//   - an https:// URL: "https://vedetta.example.com/contact"
	//
	// Apple's push relay rejects the JWT with HTTP 403 BadJwtToken if
	// the value is not a well-formed mailto or https form — values like
	// "vedetta@localhost" fail because the domain is not routable.
	//
	// Defaults to "admin@example.com" with a startup warning, which
	// parses on all push services but should be replaced in production.
	VAPIDSubscriber string `yaml:"vapid_subscriber"`
}

// DefaultVAPIDSubscriber is the fallback used when NotificationsConfig
// does not specify one. "example.com" is IANA-reserved for documentation,
// parses cleanly on every push service, and is a visible marker that the
// operator has not configured their real contact.
const DefaultVAPIDSubscriber = "admin@example.com"

// CodecsConfig controls optional external codec behavior.
type CodecsConfig struct {
	OpenH264 OpenH264Config `yaml:"openh264"`

	// HWAccel selects the H.264 decode backend for detection and snapshots.
	// "auto" (default, or empty) uses hardware decode when available and falls
	// back to the bundled software decoder; "software" forces software decode;
	// "videotoolbox" forces macOS VideoToolbox. Unknown values are rejected at
	// load time.
	HWAccel string `yaml:"hwaccel"`
}

// Valid hwaccel values. Kept in sync with media.HWAccel; validated here so the
// config layer never depends on the media package. vaapi/nvdec are accepted on
// every build but only function in binaries compiled with the matching build
// tag (see Dockerfile.hwaccel / make build-hwaccel).
const (
	HWAccelAuto         = "auto"
	HWAccelSoftware     = "software"
	HWAccelVideoToolbox = "videotoolbox"
	HWAccelVAAPI        = "vaapi"
	HWAccelNVDEC        = "nvdec"
)

// validHWAccel reports whether s is an accepted codecs.hwaccel value. An empty
// string is accepted and treated as "auto" downstream.
func validHWAccel(s string) bool {
	switch s {
	case "", HWAccelAuto, HWAccelSoftware, HWAccelVideoToolbox, HWAccelVAAPI, HWAccelNVDEC:
		return true
	default:
		return false
	}
}

// WebRTCConfig controls the ICE servers offered to WebRTC viewers.
type WebRTCConfig struct {
	// ICEServers is the explicit list of STUN/TURN servers advertised to
	// browsers during WebRTC negotiation. When empty (the default), Vedetta
	// offers no external ICE servers: LAN viewers connect via host candidates
	// and remote viewers use MSE/HLS over the TLS tunnel. This keeps every
	// viewer's IP from leaking to a third-party STUN operator. Operators who
	// need UDP-forwarded remote WebRTC opt in by listing their own servers.
	ICEServers []ICEServerConfig `yaml:"ice_servers"`
}

// ICEServerConfig describes a single STUN or TURN server. Username and
// Credential are only meaningful for TURN; STUN entries leave them empty.
type ICEServerConfig struct {
	URLs       []string `yaml:"urls" json:"urls"`
	Username   string   `yaml:"username,omitempty" json:"username,omitempty"`
	Credential string   `yaml:"credential,omitempty" json:"credential,omitempty"`
}

// TracingConfig controls opt-in OpenTelemetry distributed tracing. Disabled by
// default: when Enabled is false, vedetta installs a no-op tracer with zero
// exporter and zero overhead. Endpoint may be a scheme-less host:port (paired
// with Insecure) or a full URL; when empty the standard
// OTEL_EXPORTER_OTLP_ENDPOINT environment variable is used.
type TracingConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Endpoint    string `yaml:"endpoint"`
	Protocol    string `yaml:"protocol"`
	Insecure    bool   `yaml:"insecure"`
	ServiceName string `yaml:"service_name"`
	// Headers are attached to every OTLP trace export request. The common use is
	// a tenant header for a multi-tenant backend, e.g. X-Scope-OrgID for Tempo.
	Headers map[string]string `yaml:"headers"`
}

// LoggingConfig controls opt-in OTLP log export to a collector (e.g. for Loki).
// Disabled by default: when Enabled is false, vedetta logs only to stdout as
// before. When Endpoint is empty it reuses the tracing endpoint; if that is
// also empty the OTLP exporter resolves the endpoint from the standard
// OTEL_EXPORTER_OTLP_LOGS_ENDPOINT / OTEL_EXPORTER_OTLP_ENDPOINT env vars.
type LoggingConfig struct {
	Enabled bool `yaml:"enabled"`
	// Level is the minimum severity vedetta emits: "debug", "info", "warn" or
	// "error". Empty means "info". It applies uniformly to stdout, the rotating
	// file, and the OTLP export arm, so raising verbosity to diagnose an issue
	// cannot leave one sink quietly at a different threshold.
	Level       string `yaml:"level"`
	Endpoint    string `yaml:"endpoint"`
	Protocol    string `yaml:"protocol"`
	Insecure    bool   `yaml:"insecure"`
	ServiceName string `yaml:"service_name"`
	// Headers are attached to every OTLP log export request. The common use is a
	// tenant header for a multi-tenant backend, e.g. X-Scope-OrgID for Loki.
	Headers map[string]string `yaml:"headers"`

	// File, when set, makes vedetta write its own logs to this path with
	// built-in size-based rotation instead of stdout. Leave empty (the default)
	// to log to stdout, which the process supervisor captures. MaxSizeMB and
	// MaxBackups bound on-disk growth so the log can never grow without bound.
	File       string `yaml:"file"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxBackups int    `yaml:"max_backups"`
}

// OpenH264Config controls OpenH264 codec auto-install behavior.
type OpenH264Config struct {
	// AutoInstall downloads the Cisco-provided OpenH264 library on startup
	// when the system copy is missing. Idempotent — does nothing if already
	// available. Default: true.
	AutoInstall *bool `yaml:"auto_install"`
}

// ShouldAutoInstall returns true when auto-install is enabled (default) or
// explicitly set to true. Returns false only when explicitly disabled.
func (c OpenH264Config) ShouldAutoInstall() bool {
	return c.AutoInstall == nil || *c.AutoInstall
}

type CameraConfig struct {
	Name          string                    `yaml:"name"`
	URL           string                    `yaml:"url"`
	RecordURL     string                    `yaml:"record_url"`     // Separate high-res stream for recording (optional, defaults to URL)
	RTSPTransport string                    `yaml:"rtsp_transport"` // RTSP lower transport: "tcp" (default), "udp", or "auto". UDP avoids the TCP-interleaving desync some cameras cause on high-bitrate streams.
	Detect        DetectStreamConfig        `yaml:"detect"`
	Record        StreamConfig              `yaml:"record"`
	Zones         []Zone                    `yaml:"zones"`
	Enabled       *bool                     `yaml:"enabled"`
	Doorbell      DoorbellConfig            `yaml:"doorbell"`
	TieredStorage CameraTieredStorageConfig `yaml:"tiered_storage"`
	RetainDays    *int                      `yaml:"retain_days"` // Per-camera override for recording.retain_days; nil means use global value.
	OnDemand      bool                      `yaml:"on_demand"`   // Stream is published only part of the time (battery/PIR camera); absence is normal, not a fault.
}

type DoorbellConfig struct {
	Enabled         bool   `yaml:"enabled"`
	WebhookURL      string `yaml:"webhook_url"`      // external webhook to call on press (optional)
	ClipSeconds     *int   `yaml:"clip_seconds"`     // per-camera override of doorbell.clip_seconds
	DebounceSeconds *int   `yaml:"debounce_seconds"` // per-camera override of doorbell.debounce_seconds
}

// DoorbellDefaultsConfig holds global doorbell tuning, overridable per camera.
type DoorbellDefaultsConfig struct {
	ClipSeconds     int `yaml:"clip_seconds"`     // synthetic-end window after a press
	DebounceSeconds int `yaml:"debounce_seconds"` // collapse bouncy presses into one ring
}

func (c CameraConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func (c CameraConfig) DetectEnabled() bool {
	return c.Detect.Enabled == nil || *c.Detect.Enabled
}

type StreamConfig struct {
	Width  int `yaml:"width" json:"width"`
	Height int `yaml:"height" json:"height"`
	FPS    int `yaml:"fps" json:"fps"`
}

type DetectStreamConfig struct {
	Width   int   `yaml:"width" json:"width"`
	Height  int   `yaml:"height" json:"height"`
	FPS     int   `yaml:"fps" json:"fps"`
	Enabled *bool `yaml:"enabled" json:"enabled"`
}

type Zone struct {
	Name            string      `yaml:"name"`
	Points          [][]float64 `yaml:"points"`
	Labels          []string    `yaml:"labels"`
	TrackPresence   bool        `yaml:"track_presence"`
	FaceRecognition bool        `yaml:"face_recognition"`
}

type DetectConfig struct {
	ModelPath            string       `yaml:"model_path"`
	Backend              string       `yaml:"backend"` // "auto" (default), "go", or "onnxruntime_c"
	ScoreThreshold       float32      `yaml:"score_threshold"`
	Motion               MotionConfig `yaml:"motion"`
	Labels               []string     `yaml:"labels"`                 // Only emit events for these labels; empty = all
	ObjectMatchThreshold float64      `yaml:"object_match_threshold"` // Cosine similarity threshold for object re-ID (0.0-1.0)
}

type MotionConfig struct {
	PixelThreshold  uint8   `yaml:"pixel_threshold"`
	MinArea         int     `yaml:"min_area"`
	BackgroundAlpha float64 `yaml:"background_alpha"`
	MinRegionScore  float64 `yaml:"min_region_score"`
}

type RecordingConfig struct {
	Path             string              `yaml:"path"`
	PreCapture       time.Duration       `yaml:"pre_capture"`
	PostCapture      time.Duration       `yaml:"post_capture"`
	MaxEventDuration time.Duration       `yaml:"max_event_duration"` // Cap on dynamic event clip length (default 2m)
	RetainDays       int                 `yaml:"retain_days"`
	EventRetain      int                 `yaml:"event_retain_days"` // Keep event clips longer than continuous
	SegmentLength    time.Duration       `yaml:"segment_length"`
	Continuous       bool                `yaml:"continuous"`    // Record continuously, not just events
	MaxStorage       string              `yaml:"max_storage"`   // Human-readable max storage (e.g. "10GB", "500MB"); 0 or empty = unlimited
	MinDiskFree      string              `yaml:"min_disk_free"` // Hard threshold below which recording pauses (e.g. "2GB"); 0 or empty = no limit
	UrgentCleanup    UrgentCleanupConfig `yaml:"urgent_cleanup"`
	TieredStorage    TieredStorageConfig `yaml:"tiered_storage"`
	maxStorageBytes  int64
	minDiskFreeBytes int64
}

// UrgentCleanupConfig controls emergency retention-floor-breaking disk cleanup.
type UrgentCleanupConfig struct {
	// Enabled allows the retention floor to be broken when disk usage is critical.
	Enabled bool `yaml:"enabled"`
	// MinRetention is the minimum amount of recording to preserve even during emergency cleanup.
	MinRetention time.Duration `yaml:"min_retention"`
	// BatchSize is the number of oldest segments to drop per emergency cleanup pass.
	BatchSize int `yaml:"batch_size"`
}

// TieredStorageConfig controls scheduled overnight recompression of old segments
// and event clips.
type TieredStorageConfig struct {
	Enabled           bool          `yaml:"enabled"`
	AfterDays         int           `yaml:"after_days"`
	TargetWidth       int           `yaml:"target_width"`
	TargetHeight      int           `yaml:"target_height"`
	Schedule          string        `yaml:"schedule"`                      // "HH:MM-HH:MM", local time, may span midnight
	Interval          time.Duration `yaml:"interval"`                      // How often the recompress worker ticks inside the schedule window
	Priority          string        `yaml:"priority"`                      // "largest" | "oldest" — which eligible artifact to recompress first
	RepairClipsBefore time.Time     `yaml:"repair_clips_before,omitempty"` // Prioritize only event clips ending before this fixed RFC3339 cutoff.
}

// CameraTieredStorageConfig holds per-camera overrides for tiered storage.
// Nil pointer fields inherit from the global TieredStorageConfig.
type CameraTieredStorageConfig struct {
	Enabled   *bool `yaml:"enabled"`
	AfterDays *int  `yaml:"after_days"`
}

// MaxStorageBytes returns the parsed max storage limit in bytes.
func (r *RecordingConfig) MaxStorageBytes() int64 {
	return r.maxStorageBytes
}

// MinDiskFreeBytes returns the parsed minimum free disk space threshold in bytes.
// Recording pauses when free space falls below this value. Returns 0 when not configured.
func (r *RecordingConfig) MinDiskFreeBytes() int64 {
	return r.minDiskFreeBytes
}

// EffectiveRetainDays returns this camera's retain_days if set, otherwise the global value.
func (c CameraConfig) EffectiveRetainDays(globalRetain int) int {
	if c.RetainDays != nil && *c.RetainDays > 0 {
		return *c.RetainDays
	}
	return globalRetain
}

// EffectiveDoorbellClipSeconds returns the per-camera clip window if set, else the global.
func (c CameraConfig) EffectiveDoorbellClipSeconds(global int) int {
	if c.Doorbell.ClipSeconds != nil {
		return *c.Doorbell.ClipSeconds
	}
	return global
}

// EffectiveDoorbellDebounceSeconds returns the per-camera debounce if set, else the global.
// A pointer is used so an explicit 0 (debounce disabled) is distinguishable from unset.
func (c CameraConfig) EffectiveDoorbellDebounceSeconds(global int) int {
	if c.Doorbell.DebounceSeconds != nil {
		return *c.Doorbell.DebounceSeconds
	}
	return global
}

type EventConfig struct {
	CooldownSeconds int    `yaml:"cooldown_seconds"`
	RetainDays      int    `yaml:"retain_days"`
	SnapshotPath    string `yaml:"snapshot_path"`
	SnapshotQuality int    `yaml:"snapshot_quality"`
}

type StorageConfig struct {
	DBPath string `yaml:"db_path"`
}

type MQTTConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Topic    string `yaml:"topic"`
}

type APIConfig struct {
	Host           string   `yaml:"host"`
	Port           int      `yaml:"port"`
	Exposure       string   `yaml:"exposure"`
	AllowedOrigins []string `yaml:"allowed_origins"`
	TrustedProxies []string `yaml:"trusted_proxies"`
	TLSCert        string   `yaml:"tls_cert"` // path to TLS certificate file (enables HTTPS)
	TLSKey         string   `yaml:"tls_key"`  // path to TLS private key file
}

type RTSPServerConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

type ProxyAuthConfig struct {
	Header string `yaml:"header"`
}

type AuthConfig struct {
	Users []AuthUser      `yaml:"users"`
	Proxy ProxyAuthConfig `yaml:"proxy"`
}

type UpdateConfig struct {
	CheckEnabled  bool          `yaml:"check_enabled"`
	CheckInterval time.Duration `yaml:"check_interval"`
}

type AuthUser struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}

const MaxCameraNameLength = 64

// ValidateCameraName enforces names that are safe as identifiers and path
// components. Camera display labels can be handled separately by the UI.
func ValidateCameraName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if len(name) > MaxCameraNameLength {
		return fmt.Errorf("name must be at most %d characters", MaxCameraNameLength)
	}
	for i, r := range name {
		if i == 0 && !isASCIIAlnum(r) {
			return fmt.Errorf("name must start with a letter or digit")
		}
		if !isASCIIAlnum(r) && r != '_' && r != '-' {
			return fmt.Errorf("name may only contain letters, digits, underscores, and hyphens")
		}
	}
	return nil
}

// SanitizeCameraName converts discovery/display names into safe config names.
func SanitizeCameraName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastSep := false
	for _, r := range name {
		var out byte
		switch {
		case r >= 'a' && r <= 'z':
			out = byte(r)
		case r >= '0' && r <= '9':
			out = byte(r)
		case r == ' ', r == '_', r == '-', r == '.':
			out = '_'
		case r >= 'A' && r <= 'Z':
			out = byte(r + ('a' - 'A'))
		default:
			continue
		}
		if out == '_' {
			if b.Len() == 0 || lastSep {
				continue
			}
			lastSep = true
		} else {
			lastSep = false
		}
		b.WriteByte(out)
		if b.Len() >= MaxCameraNameLength {
			break
		}
	}
	result := strings.Trim(b.String(), "_-")
	if result == "" {
		return "camera"
	}
	return result
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// Defaults returns a Config populated with all default values.
func Defaults() *Config {
	return &Config{
		Detect: DetectConfig{
			ScoreThreshold:       0.65,
			ObjectMatchThreshold: 0.75,
			Motion: MotionConfig{
				PixelThreshold:  25,
				MinArea:         200,
				BackgroundAlpha: 0.05,
				MinRegionScore:  0.02,
			},
			Labels: []string{"person", "car", "truck", "bus", "motorcycle", "bicycle", "dog", "cat", "bird"},
		},
		Recording: RecordingConfig{
			Path:             "./recordings",
			PreCapture:       5 * time.Second,
			PostCapture:      10 * time.Second,
			MaxEventDuration: 2 * time.Minute,
			RetainDays:       7,
			EventRetain:      30,
			SegmentLength:    10 * time.Minute,
			Continuous:       true,
			MinDiskFree:      "2GB",
			UrgentCleanup: UrgentCleanupConfig{
				Enabled:      true,
				MinRetention: time.Hour,
				BatchSize:    50,
			},
			TieredStorage: TieredStorageConfig{
				Enabled:      false,
				AfterDays:    1,
				TargetWidth:  1280,
				TargetHeight: 720,
				Schedule:     "22:00-06:00",
				Interval:     30 * time.Second,
				Priority:     "largest",
			},
		},
		Events: EventConfig{
			CooldownSeconds: 30,
			RetainDays:      90,
			SnapshotPath:    "./snapshots",
			SnapshotQuality: 85,
		},
		Storage: StorageConfig{
			DBPath: "./vedetta.db",
		},
		API: APIConfig{
			Host:     "0.0.0.0",
			Port:     5050,
			Exposure: "lan",
		},
		RTSPServer: RTSPServerConfig{
			Port: 8554,
		},
		Updates: UpdateConfig{
			CheckEnabled:  true,
			CheckInterval: 24 * time.Hour,
		},
		Tracing: TracingConfig{
			Protocol:    "http",
			Insecure:    true,
			ServiceName: "vedetta",
		},
		Logging: LoggingConfig{
			Insecure:    true,
			ServiceName: "vedetta",
			MaxSizeMB:   50,
			MaxBackups:  5,
		},
		Runtime: RuntimeConfig{
			MemoryGuard:   true,
			MemoryLimitMB: 0, // 0 = auto (60% of system RAM)
		},
		Doorbell: DoorbellDefaultsConfig{
			ClipSeconds:     15,
			DebounceSeconds: 10,
		},
	}
}

// LoadOrDefault loads config from path if it exists, or returns defaults with
// setupMode=true when the file is missing. Returns an error for invalid files.
func LoadOrDefault(path string) (*Config, bool, error) {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return Defaults(), true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("checking config file: %w", err)
	}
	cfg, err := Load(path)
	if err != nil {
		return nil, false, err
	}
	return cfg, false, nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg := Defaults()

	// Strict decoding: unknown keys (typos, removed/dead options) are errors
	// rather than silently dropped, so config can never lie about what it sets.
	// An empty document yields io.EOF, which keeps the defaults above.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if !validHWAccel(cfg.Codecs.HWAccel) {
		return nil, fmt.Errorf("codecs.hwaccel: invalid value %q (want auto, software, videotoolbox, vaapi, or nvdec)", cfg.Codecs.HWAccel)
	}

	if cfg.Recording.MaxStorage != "" {
		bytes, err := parseByteSize(cfg.Recording.MaxStorage)
		if err != nil {
			return nil, fmt.Errorf("recording.max_storage: %w", err)
		}
		cfg.Recording.maxStorageBytes = bytes
	}
	if cfg.Recording.MinDiskFree != "" {
		bytes, err := parseByteSize(cfg.Recording.MinDiskFree)
		if err != nil {
			return nil, fmt.Errorf("recording.min_disk_free: %w", err)
		}
		cfg.Recording.minDiskFreeBytes = bytes
	}
	if cfg.Recording.UrgentCleanup.BatchSize <= 0 {
		cfg.Recording.UrgentCleanup.BatchSize = 50
	}
	if cfg.Recording.UrgentCleanup.MinRetention <= 0 {
		cfg.Recording.UrgentCleanup.MinRetention = time.Hour
	}
	if cfg.Recording.TieredStorage.Interval <= 0 {
		cfg.Recording.TieredStorage.Interval = 30 * time.Second
	}
	if cfg.Recording.TieredStorage.Priority == "" {
		cfg.Recording.TieredStorage.Priority = "largest"
	}
	if cfg.Recording.TieredStorage.Priority != "largest" && cfg.Recording.TieredStorage.Priority != "oldest" {
		return nil, fmt.Errorf("recording.tiered_storage.priority: must be \"largest\" or \"oldest\"")
	}

	if cfg.Doorbell.ClipSeconds <= 0 {
		cfg.Doorbell.ClipSeconds = 15
	}
	if cfg.Doorbell.DebounceSeconds < 0 {
		cfg.Doorbell.DebounceSeconds = 10
	}

	// Validate TLS config: both cert and key must be set together
	if (cfg.API.TLSCert != "") != (cfg.API.TLSKey != "") {
		return nil, fmt.Errorf("api: both tls_cert and tls_key must be set")
	}
	if cfg.API.Exposure != "lan" && cfg.API.Exposure != "internet" {
		return nil, fmt.Errorf("api.exposure: must be \"lan\" or \"internet\"")
	}
	if cfg.API.Exposure == "internet" && cfg.API.TLSCert == "" && len(cfg.API.TrustedProxies) == 0 {
		return nil, fmt.Errorf("api.exposure=internet requires tls_cert/tls_key or at least one trusted proxy")
	}
	for i, proxy := range cfg.API.TrustedProxies {
		if _, err := parseProxyPrefix(proxy); err != nil {
			return nil, fmt.Errorf("api.trusted_proxies[%d]: %w", i, err)
		}
	}
	for i, origin := range cfg.API.AllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("api.allowed_origins[%d]: must be an absolute origin", i)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("api.allowed_origins[%d]: scheme must be http or https", i)
		}
		if u.Path != "" && u.Path != "/" {
			return nil, fmt.Errorf("api.allowed_origins[%d]: origin must not include a path", i)
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("api.allowed_origins[%d]: origin must not include query or fragment", i)
		}
	}

	if cfg.Auth.Proxy.Header != "" && len(cfg.API.TrustedProxies) == 0 {
		return nil, fmt.Errorf("auth.proxy.header requires at least one api.trusted_proxies entry")
	}

	if len(cfg.Auth.Users) == 0 {
		return nil, fmt.Errorf("at least one auth user must be configured")
	}

	configDir := filepath.Dir(path)
	cfg.Storage.DBPath = normalizePath(configDir, cfg.Storage.DBPath)
	cfg.Recording.Path = normalizePath(configDir, cfg.Recording.Path)
	cfg.Events.SnapshotPath = normalizePath(configDir, cfg.Events.SnapshotPath)
	cfg.Detect.ModelPath = normalizePath(configDir, cfg.Detect.ModelPath)
	cfg.API.TLSCert = normalizePath(configDir, cfg.API.TLSCert)
	cfg.API.TLSKey = normalizePath(configDir, cfg.API.TLSKey)

	for i := range cfg.Cameras {
		cam := &cfg.Cameras[i]
		if err := ValidateCameraName(cam.Name); err != nil {
			return nil, fmt.Errorf("camera %d: %w", i, err)
		}
		if cam.URL == "" {
			return nil, fmt.Errorf("camera %q: url is required", cam.Name)
		}
		switch cam.RTSPTransport {
		case "":
			cam.RTSPTransport = "tcp"
		case "tcp", "udp", "auto":
			// valid
		default:
			return nil, fmt.Errorf("camera %q: rtsp_transport must be tcp, udp, or auto (got %q)", cam.Name, cam.RTSPTransport)
		}
		if cam.Detect.Width == 0 {
			cam.Detect.Width = 640
			cam.Detect.Height = 480
			cam.Detect.FPS = 5
		}
		if cam.Record.Width == 0 {
			cam.Record.Width = 1920
			cam.Record.Height = 1080
			cam.Record.FPS = 15
		}
		for j, z := range cam.Zones {
			if z.Name == "" {
				return nil, fmt.Errorf("camera %q: zone %d: name is required", cam.Name, j)
			}
			if len(z.Points) < 3 {
				return nil, fmt.Errorf("camera %q: zone %q: points must contain at least 3 polygon points", cam.Name, z.Name)
			}
			for _, point := range z.Points {
				if len(point) != 2 {
					return nil, fmt.Errorf("camera %q: zone %q: each point must be [x, y]", cam.Name, z.Name)
				}
				if point[0] < 0 || point[0] > 1 || point[1] < 0 || point[1] > 1 {
					return nil, fmt.Errorf("camera %q: zone %q: points must be between 0.0 and 1.0", cam.Name, z.Name)
				}
			}
		}
	}

	for i, user := range cfg.Auth.Users {
		if user.Username == "" {
			return nil, fmt.Errorf("auth.users[%d]: username is required", i)
		}
		if user.PasswordHash == "" {
			return nil, fmt.Errorf("auth.users[%d]: password_hash is required", i)
		}
	}

	// Normalize to match the runtime resolver (otelexport.ParseProtocol): trim
	// and lower-case so values like "GRPC" or " grpc " validate and are stored
	// canonically rather than rejected here but accepted at export time.
	cfg.Tracing.Protocol = strings.ToLower(strings.TrimSpace(cfg.Tracing.Protocol))
	if cfg.Tracing.Protocol == "" {
		cfg.Tracing.Protocol = "http"
	}
	switch cfg.Tracing.Protocol {
	case "http", "http/protobuf", "grpc":
	default:
		return nil, fmt.Errorf("tracing.protocol: must be \"http\", \"http/protobuf\", or \"grpc\"")
	}
	if cfg.Tracing.ServiceName == "" {
		cfg.Tracing.ServiceName = "vedetta"
	}
	for k := range cfg.Tracing.Headers {
		if strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("tracing.headers: header name must not be empty")
		}
	}

	// Unlike tracing, logging.protocol is left empty when unset. The logging
	// package's transport fallback reuses tracing's protocol only while
	// logging.protocol is empty, so filling a default here would pin logs to that
	// default and defeat the fallback. Still normalize and validate any value the
	// user did set.
	cfg.Logging.Protocol = strings.ToLower(strings.TrimSpace(cfg.Logging.Protocol))
	switch cfg.Logging.Protocol {
	case "", "http", "http/protobuf", "grpc":
	default:
		return nil, fmt.Errorf("logging.protocol: must be \"http\", \"http/protobuf\", or \"grpc\"")
	}
	if cfg.Logging.ServiceName == "" {
		cfg.Logging.ServiceName = "vedetta"
	}
	for k := range cfg.Logging.Headers {
		if strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("logging.headers: header name must not be empty")
		}
	}

	return cfg, nil
}

// parseByteSize parses human-readable byte sizes like "10GB", "500MB", "1TB".
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}

	s = strings.ToUpper(s)

	// Check longer suffixes first to avoid "B" matching "GB", "MB", etc.
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"TB", 1024 * 1024 * 1024 * 1024},
		{"GB", 1024 * 1024 * 1024},
		{"MB", 1024 * 1024},
		{"KB", 1024},
		{"B", 1},
	}

	for _, entry := range suffixes {
		if strings.HasSuffix(s, entry.suffix) {
			numStr := strings.TrimSpace(strings.TrimSuffix(s, entry.suffix))
			val, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid size %q: %w", s, err)
			}
			return int64(val * float64(entry.mult)), nil
		}
	}

	// Try as plain number (bytes)
	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: expected format like '10GB', '500MB'", s)
	}
	return val, nil
}

func normalizePath(baseDir, p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(baseDir, p))
}

func parseProxyPrefix(value string) (netip.Prefix, error) {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix, nil
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid proxy CIDR or IP %q", value)
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// ParseScheduleWindow parses a "HH:MM-HH:MM" schedule string into start and end
// clock times as minutes-since-midnight. Returns an error on invalid format.
// The window may span midnight (e.g. "23:00-01:00").
func ParseScheduleWindow(s string) (startMin, endMin int, err error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid schedule %q: expected HH:MM-HH:MM", s)
	}
	parse := func(t string) (int, error) {
		var h, m int
		if _, err := fmt.Sscanf(t, "%d:%d", &h, &m); err != nil {
			return 0, fmt.Errorf("invalid time %q", t)
		}
		if h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, fmt.Errorf("time %q out of range", t)
		}
		return h*60 + m, nil
	}
	startMin, err = parse(parts[0])
	if err != nil {
		return 0, 0, err
	}
	endMin, err = parse(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return startMin, endMin, nil
}

// InScheduleWindow reports whether now (local time) falls within the schedule window.
// Handles windows that span midnight (e.g. "23:00-01:00").
func InScheduleWindow(schedule string, now time.Time) (bool, error) {
	startMin, endMin, err := ParseScheduleWindow(schedule)
	if err != nil {
		return false, err
	}
	nowMin := now.Hour()*60 + now.Minute()
	if startMin <= endMin {
		return nowMin >= startMin && nowMin < endMin, nil
	}
	// Spans midnight
	return nowMin >= startMin || nowMin < endMin, nil
}

// EffectiveTieredStorage returns the effective tiered storage config for this camera,
// applying per-camera overrides on top of the global config.
func (c CameraConfig) EffectiveTieredStorage(global TieredStorageConfig) TieredStorageConfig {
	result := global
	if c.TieredStorage.Enabled != nil {
		result.Enabled = *c.TieredStorage.Enabled
	}
	if c.TieredStorage.AfterDays != nil {
		result.AfterDays = *c.TieredStorage.AfterDays
	}
	return result
}
