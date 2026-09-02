package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ErrDuplicateCameraName reports a write refused because the config already
// carries that name. It is separated from the I/O failures around it so an API
// caller can answer with the operator's mistake instead of a server error:
// the correction is obvious and belongs in the response.
var ErrDuplicateCameraName = errors.New("camera name already in use")

// yamlConfig mirrors Config but uses string durations for human-readable YAML output.
// time.Duration fields marshal as nanoseconds by default, so this struct ensures
// durations like "10m" and "5s" appear in the generated YAML.
type yamlConfig struct {
	Auth      yamlAuth      `yaml:"auth"`
	API       APIConfig     `yaml:"api"`
	Storage   StorageConfig `yaml:"storage"`
	Recording yamlRecording `yaml:"recording"`
	Events    EventConfig   `yaml:"events"`
	Detect    yamlDetect    `yaml:"detect"`
}

type yamlAuth struct {
	Users []AuthUser `yaml:"users"`
}

type yamlRecording struct {
	Path          string `yaml:"path"`
	Continuous    bool   `yaml:"continuous"`
	SegmentLength string `yaml:"segment_length"`
	PreCapture    string `yaml:"pre_capture"`
	PostCapture   string `yaml:"post_capture"`
	RetainDays    int    `yaml:"retain_days"`
	EventRetain   int    `yaml:"event_retain_days"`
}

type yamlDetect struct {
	ScoreThreshold float32 `yaml:"score_threshold"`
}

// WriteInitialConfig writes a new config.yml with auth credentials and all defaults.
func WriteInitialConfig(path, username, passwordHash string) error {
	defer lockConfig(path)()

	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config already exists")
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("checking config: %w", err)
	}

	content, err := GenerateInitialConfigYAML(username, passwordHash)
	if err != nil {
		return fmt.Errorf("generating config: %w", err)
	}
	return writeConfigFile(path, []byte(content), 0600)
}

// GenerateInitialConfigYAML returns the YAML string for an initial config with
// auth credentials and default values. The output is loadable by Load().
func GenerateInitialConfigYAML(username, passwordHash string) (string, error) {
	cfg := yamlConfig{
		Auth: yamlAuth{
			Users: []AuthUser{
				{Username: username, PasswordHash: passwordHash},
			},
		},
		API: APIConfig{
			Host:     "0.0.0.0",
			Port:     5050,
			Exposure: "lan",
		},
		Storage: StorageConfig{
			DBPath: "./vedetta.db",
		},
		Recording: yamlRecording{
			Path:          "./recordings",
			Continuous:    true,
			SegmentLength: "10m",
			PreCapture:    "5s",
			PostCapture:   "10s",
			RetainDays:    7,
			EventRetain:   30,
		},
		Events: EventConfig{
			CooldownSeconds: 30,
			RetainDays:      90,
			SnapshotPath:    "./snapshots",
			SnapshotQuality: 85,
		},
		Detect: yamlDetect{
			ScoreThreshold: 0.65,
		},
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return "", fmt.Errorf("encoding config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("closing encoder: %w", err)
	}
	return buf.String(), nil
}

// updateConfigSection reads the config file as a yaml.Node tree, finds or creates
// the given top-level key, replaces its value with the provided struct, and writes
// the file back, preserving existing structure and comments. The caller holds the
// path's config lock.
func updateConfigSection(path, sectionKey string, value any) error {
	doc, root, err := readConfigDocument(path)
	if err != nil {
		return err
	}

	var valueNode yaml.Node
	if err := valueNode.Encode(value); err != nil {
		return fmt.Errorf("marshaling %s: %w", sectionKey, err)
	}

	found := false
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value == sectionKey {
			root.Content[i+1] = &valueNode
			found = true
			break
		}
	}
	if !found {
		keyNode := &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!str",
			Value: sectionKey,
		}
		root.Content = append(root.Content, keyNode, &valueNode)
	}

	return writeDocToFile(path, doc)
}

// updateConfigSectionFields merges the encoded fields into an existing mapping
// instead of replacing the whole section. Unedited and unknown fields retain
// their yaml.Node values and comments. The caller holds the path's config lock.
func updateConfigSectionFields(path, sectionKey string, value any) error {
	doc, root, err := readConfigDocument(path)
	if err != nil {
		return err
	}

	var fields yaml.Node
	if err := fields.Encode(value); err != nil {
		return fmt.Errorf("marshaling %s: %w", sectionKey, err)
	}
	if fields.Kind != yaml.MappingNode {
		return fmt.Errorf("marshaling %s: expected mapping node", sectionKey)
	}

	var section *yaml.Node
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value == sectionKey {
			section = root.Content[i+1]
			break
		}
	}
	if section == nil {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: sectionKey}
		root.Content = append(root.Content, keyNode, &fields)
	} else {
		if section.Kind != yaml.MappingNode {
			return fmt.Errorf("unexpected YAML structure: %s must be a mapping", sectionKey)
		}
		for i := 0; i < len(fields.Content)-1; i += 2 {
			fieldKey := fields.Content[i]
			fieldValue := fields.Content[i+1]
			updated := false
			for j := 0; j < len(section.Content)-1; j += 2 {
				if section.Content[j].Value == fieldKey.Value {
					section.Content[j+1] = fieldValue
					updated = true
					break
				}
			}
			if !updated {
				section.Content = append(section.Content, fieldKey, fieldValue)
			}
		}
	}

	return writeDocToFile(path, doc)
}

// UpdateMQTT updates the mqtt section of the config file.
func UpdateMQTT(path string, mqtt MQTTConfig) error {
	defer lockConfig(path)()
	return updateConfigSection(path, "mqtt", mqtt)
}

// yamlUpdateConfig uses string for duration fields so YAML output is human-readable.
type yamlUpdateConfig struct {
	CheckEnabled  bool   `yaml:"check_enabled"`
	CheckInterval string `yaml:"check_interval"`
}

// UpdateUpdates updates the updates section of the config file.
func UpdateUpdates(path string, updates UpdateConfig) error {
	defer lockConfig(path)()
	y := yamlUpdateConfig{
		CheckEnabled:  updates.CheckEnabled,
		CheckInterval: updates.CheckInterval.String(),
	}
	return updateConfigSection(path, "updates", y)
}

// yamlRecordingWrite uses string durations for human-readable YAML output.
type yamlRecordingWrite struct {
	Path          string `yaml:"path"`
	Continuous    bool   `yaml:"continuous"`
	SegmentLength string `yaml:"segment_length"`
	PreCapture    string `yaml:"pre_capture"`
	PostCapture   string `yaml:"post_capture"`
	RetainDays    int    `yaml:"retain_days"`
	EventRetain   int    `yaml:"event_retain_days"`
	MaxStorage    string `yaml:"max_storage"`
}

// UpdateRecording updates the UI-editable recording fields while preserving
// advanced and unknown fields in the same section.
func UpdateRecording(path string, rec RecordingConfig) error {
	defer lockConfig(path)()
	y := yamlRecordingWrite{
		Path:          rec.Path,
		Continuous:    rec.Continuous,
		SegmentLength: rec.SegmentLength.String(),
		PreCapture:    rec.PreCapture.String(),
		PostCapture:   rec.PostCapture.String(),
		RetainDays:    rec.RetainDays,
		EventRetain:   rec.EventRetain,
		MaxStorage:    rec.MaxStorage,
	}

	return updateConfigSectionFields(path, "recording", y)
}

// UpdateDetect updates only the UI-editable fields of the detect section
// (score_threshold and labels) while preserving all other fields such as
// model_path, backend, motion, and object_match_threshold.
func UpdateDetect(path string, detect DetectConfig) error {
	defer lockConfig(path)()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	var existing struct {
		Detect map[string]any `yaml:"detect"`
	}
	if err := yaml.Unmarshal(data, &existing); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	if existing.Detect == nil {
		existing.Detect = make(map[string]any)
	}
	existing.Detect["score_threshold"] = detect.ScoreThreshold
	if len(detect.Labels) > 0 {
		existing.Detect["labels"] = detect.Labels
	} else {
		delete(existing.Detect, "labels")
	}
	return updateConfigSection(path, "detect", existing.Detect)
}

// AppendCamera adds a camera to an existing config file using yaml.Node to
// preserve the existing document structure (comments, ordering, other sections).
func AppendCamera(path string, cam CameraConfig, comment string) error {
	if err := ValidateCameraName(cam.Name); err != nil {
		return fmt.Errorf("invalid camera name: %w", err)
	}
	if cam.URL == "" {
		return fmt.Errorf("camera url is required")
	}

	defer lockConfig(path)()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	// doc is a Document node; its first Content is the root mapping
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("unexpected YAML structure: expected document node")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("unexpected YAML structure: expected mapping node")
	}

	// Find or create the "cameras" key in the root mapping
	var camerasSeq *yaml.Node
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value == "cameras" {
			camerasSeq = root.Content[i+1]
			break
		}
	}

	if camerasSeq == nil {
		// Create "cameras" key and empty sequence
		keyNode := &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!str",
			Value: "cameras",
		}
		seqNode := &yaml.Node{
			Kind: yaml.SequenceNode,
			Tag:  "!!seq",
		}
		root.Content = append(root.Content, keyNode, seqNode)
		camerasSeq = seqNode
	}

	// Reject a name the config already carries, applying the same rule Load
	// enforces so an accepted append cannot produce a file the next start
	// refuses to read.
	for _, existing := range camerasSeq.Content {
		nameNode := findMappingValue(existing, "name")
		if nameNode == nil {
			continue
		}
		if sameCameraName(nameNode.Value, cam.Name) {
			return fmt.Errorf("%w: %q is already configured as %q", ErrDuplicateCameraName, cam.Name, nameNode.Value)
		}
	}

	// Marshal the camera to a yaml.Node
	camNode, err := marshalCameraNode(cam, comment)
	if err != nil {
		return fmt.Errorf("marshaling camera: %w", err)
	}

	camerasSeq.Content = append(camerasSeq.Content, camNode)

	return writeDocToFile(path, &doc)
}

// GenerateCameraYAML returns a YAML snippet for a camera configuration.
func GenerateCameraYAML(cam CameraConfig, comment string) (string, error) {
	if err := ValidateCameraName(cam.Name); err != nil {
		return "", fmt.Errorf("invalid camera name: %w", err)
	}
	if cam.URL == "" {
		return "", fmt.Errorf("camera url is required")
	}

	camNode, err := marshalCameraNode(cam, comment)
	if err != nil {
		return "", fmt.Errorf("marshaling camera: %w", err)
	}

	// Wrap in a sequence for proper YAML list output
	seqNode := &yaml.Node{
		Kind:    yaml.SequenceNode,
		Tag:     "!!seq",
		Content: []*yaml.Node{camNode},
	}

	// Wrap in a mapping with "cameras" key
	mapNode := &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "cameras"},
			seqNode,
		},
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(mapNode); err != nil {
		return "", fmt.Errorf("encoding camera: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("closing encoder: %w", err)
	}
	return buf.String(), nil
}

// findMappingValue finds the value node for a key in a mapping node.
func findMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// readConfigDocument loads and validates the common document shape used by
// every field-preserving config edit.
func readConfigDocument(path string) (doc, root *yaml.Node, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading config: %w", err)
	}

	doc = &yaml.Node{}
	if err := yaml.Unmarshal(data, doc); err != nil {
		return nil, nil, fmt.Errorf("parsing config: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil, fmt.Errorf("unexpected YAML structure: expected document node")
	}
	root = doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("unexpected YAML structure: expected mapping node")
	}
	return doc, root, nil
}

// writeDocToFile encodes a yaml.Node document and replaces the file at the given
// path with it, preserving the file's existing permissions. The caller holds the
// path's config lock.
func writeDocToFile(path string, doc *yaml.Node) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("closing encoder: %w", err)
	}
	mode, err := configFileMode(path)
	if err != nil {
		return err
	}
	return writeConfigFile(path, buf.Bytes(), mode)
}

// UpdateCamera replaces the camera entry at the given index in the cameras
// sequence with the provided CameraConfig.
func UpdateCamera(path string, index int, cam CameraConfig) error {
	if err := ValidateCameraName(cam.Name); err != nil {
		return fmt.Errorf("invalid camera name: %w", err)
	}
	if cam.URL == "" {
		return fmt.Errorf("camera url is required")
	}

	defer lockConfig(path)()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("unexpected YAML structure")
	}
	root := doc.Content[0]
	camerasSeq := findMappingValue(root, "cameras")
	if camerasSeq == nil || camerasSeq.Kind != yaml.SequenceNode {
		return fmt.Errorf("cameras section not found")
	}
	if index < 0 || index >= len(camerasSeq.Content) {
		return fmt.Errorf("camera index %d out of range (have %d cameras)", index, len(camerasSeq.Content))
	}

	// A rename must obey the same uniqueness rule Load enforces, or the write
	// succeeds and the next start refuses the file it just produced. The entry
	// being replaced is skipped so an edit that leaves the name alone is not
	// rejected by its own name.
	for i, existing := range camerasSeq.Content {
		if i == index {
			continue
		}
		nameNode := findMappingValue(existing, "name")
		if nameNode == nil {
			continue
		}
		if sameCameraName(nameNode.Value, cam.Name) {
			return fmt.Errorf("%w: %q is already configured as %q", ErrDuplicateCameraName, cam.Name, nameNode.Value)
		}
	}

	var camNode yaml.Node
	if err := camNode.Encode(cam); err != nil {
		return fmt.Errorf("marshaling camera: %w", err)
	}
	camerasSeq.Content[index] = &camNode
	return writeDocToFile(path, &doc)
}

// RemoveCamera removes the camera entry at the given index from the cameras
// sequence, shifting subsequent entries down.
func RemoveCamera(path string, index int) error {
	defer lockConfig(path)()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("unexpected YAML structure")
	}
	root := doc.Content[0]
	camerasSeq := findMappingValue(root, "cameras")
	if camerasSeq == nil || camerasSeq.Kind != yaml.SequenceNode {
		return fmt.Errorf("cameras section not found")
	}
	if index < 0 || index >= len(camerasSeq.Content) {
		return fmt.Errorf("camera index %d out of range (have %d cameras)", index, len(camerasSeq.Content))
	}
	camerasSeq.Content = append(camerasSeq.Content[:index], camerasSeq.Content[index+1:]...)
	return writeDocToFile(path, &doc)
}

// UpdateAuthPassword updates the password_hash for the given username in the
// auth.users section of the config file.
func UpdateAuthPassword(path string, username, newHash string) error {
	defer lockConfig(path)()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("unexpected YAML structure")
	}
	root := doc.Content[0]
	authMap := findMappingValue(root, "auth")
	if authMap == nil || authMap.Kind != yaml.MappingNode {
		return fmt.Errorf("auth section not found")
	}
	usersSeq := findMappingValue(authMap, "users")
	if usersSeq == nil || usersSeq.Kind != yaml.SequenceNode {
		return fmt.Errorf("auth.users section not found")
	}
	for _, userNode := range usersSeq.Content {
		if userNode.Kind != yaml.MappingNode {
			continue
		}
		nameVal := findMappingValue(userNode, "username")
		if nameVal != nil && nameVal.Value == username {
			hashVal := findMappingValue(userNode, "password_hash")
			if hashVal != nil {
				hashVal.Value = newHash
				return writeDocToFile(path, &doc)
			}
		}
	}
	return fmt.Errorf("user %q not found in config", username)
}

// marshalCameraNode creates a yaml.Node for a CameraConfig, adding a head
// comment if provided.
func marshalCameraNode(cam CameraConfig, comment string) (*yaml.Node, error) {
	// Build a minimal struct with only set fields for cleaner YAML
	camYAML := struct {
		Name      string `yaml:"name"`
		URL       string `yaml:"url"`
		RecordURL string `yaml:"record_url,omitempty"`
	}{
		Name:      cam.Name,
		URL:       cam.URL,
		RecordURL: cam.RecordURL,
	}

	var camNode yaml.Node
	if err := camNode.Encode(camYAML); err != nil {
		return nil, err
	}

	if comment != "" {
		camNode.HeadComment = comment
	}

	return &camNode, nil
}
