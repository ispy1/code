package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/twmb/franz-go/pkg/kgo"
)

// ==========================================
// 0. Configuration
// ==========================================

type Config struct {
	Server         ServerConfig         `yaml:"server"`
	Kafka          KafkaConfig          `yaml:"kafka"`
	Connect        ConnectConfig        `yaml:"connect"`
	SchemaRegistry SchemaRegistryConfig `yaml:"schema_registry"`
}

type ServerConfig struct {
	Port                 int `yaml:"port"`
	RefreshIntervalHours int `yaml:"refresh_interval_hours"`
}

type KafkaConfig struct {
	Brokers []string `yaml:"brokers"`
}

type ConnectConfig struct {
	URL string    `yaml:"url"`
	TLS TLSConfig `yaml:"tls"`
}

type TLSConfig struct {
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
	CAFile   string `yaml:"ca_file,omitempty"`
}

type SchemaRegistryConfig struct {
	URL       string    `yaml:"url"`
	TLS       TLSConfig `yaml:"tls"`
	APIKey    string    `yaml:"api_key"`
	APISecret string    `yaml:"api_secret"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ==========================================
// 1. Data Structures
// ==========================================

type TopicMetadata struct {
	TopicRealName       string    `json:"topic_real_name"`
	K8sResourceName     string    `json:"k8s_resource_name"`
	LastTopologyRefresh time.Time `json:"last_topology_refresh"`
}

type TopicSource struct {
	Name       string `json:"name"`
	SMTApplied bool   `json:"smt_applied"`
	Mode       string `json:"mode"`
}

type TopicTopology struct {
	Source TopicSource `json:"source"`
	Sinks  []string    `json:"sinks"`
}

type TopicSchema struct {
	Subject  string `json:"subject"`
	Versions []int  `json:"versions"`
}

type CachedTopic struct {
	Metadata TopicMetadata `json:"metadata"`
	Topology TopicTopology `json:"topology"`
	Schema   TopicSchema   `json:"schema"`
}

type RuntimeStatus struct {
	FetchedAt      time.Time         `json:"fetched_at"`
	PartitionCount int               `json:"partition_count,omitempty"`
	TotalOffset    int64             `json:"total_offset,omitempty"`
	Partitions     []PartitionDetail `json:"partitions,omitempty"`
	Error          string            `json:"error,omitempty"`
}

type PartitionDetail struct {
	ID     int32 `json:"id"`
	Offset int64 `json:"offset"`
}

type APIResponse struct {
	Metadata TopicMetadata `json:"metadata"`
	Topology TopicTopology `json:"topology"`
	Schema   TopicSchema   `json:"schema"`
	Runtime  RuntimeStatus `json:"runtime"`
}

// ==========================================
// 2. Thread-Safe Cache (atomic.Value)
// ==========================================

type TopologyCache struct {
	data       atomic.Value
	lastUpdate time.Time
}

func NewTopologyCache() *TopologyCache {
	tc := &TopologyCache{}
	tc.data.Store(make(map[string]CachedTopic))
	return tc
}

func (tc *TopologyCache) Get(topicName string) (CachedTopic, bool) {
	m := tc.data.Load().(map[string]CachedTopic)
	detail, exists := m[topicName]
	return detail, exists
}

func (tc *TopologyCache) Replace(newTopology map[string]CachedTopic) {
	tc.data.Store(newTopology)
	tc.lastUpdate = time.Now()
}

func (tc *TopologyCache) Count() int {
	m := tc.data.Load().(map[string]CachedTopic)
	return len(m)
}

// ==========================================
// 3. Strategy Pattern for Matchers
// ==========================================

type ConnectorMatcher interface {
	Match(topic string, config map[string]string) bool
}

type DebeziumMatcher struct{}

func (m *DebeziumMatcher) Match(topic string, config map[string]string) bool {
	prefix := config["topic.prefix"]
	if prefix != "" && strings.HasPrefix(topic, prefix) {
		return true
	}
	return false
}

type DefaultMatcher struct{}

func (m *DefaultMatcher) Match(topic string, config map[string]string) bool {
	return false
}

func MatcherFactory(class string) ConnectorMatcher {
	switch {
	case strings.Contains(class, "io.debezium.connector"):
		return &DebeziumMatcher{}
	default:
		return &DefaultMatcher{}
	}
}

// ==========================================
// 4. mTLS HTTP Client & Kafka Live Fetch
// ==========================================

func newMTLSClient(certFile, keyFile, caFile string) (*http.Client, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load client cert/key: %v", err)
	}

	caCert, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %v", err)
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caCertPool,
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}, nil
}

func fetchLiveKafkaStatus(client *kgo.Client, topic string) RuntimeStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status := RuntimeStatus{
		FetchedAt: time.Now(),
	}

	// Using franz-go to fetch end offsets.
	// client.ListEndOffsets returns map[string]map[int32]kgo.Offset
	offsets, err := client.ListEndOffsets(ctx, topic)
	if err != nil {
		status.Error = fmt.Sprintf("kafka live fetch failed: %v", err)
		return status
	}

	topicOffsets, exists := offsets[topic]
	if !exists {
		status.Error = "topic not found in kafka cluster"
		return status
	}

	var totalOffset int64
	for partitionID, offsetResp := range topicOffsets {
		if offsetResp.Err != nil {
			continue // Skip partitions with errors
		}
		status.Partitions = append(status.Partitions, PartitionDetail{
			ID:     partitionID,
			Offset: offsetResp.Offset,
		})
		totalOffset += offsetResp.Offset
	}

	status.PartitionCount = len(status.Partitions)
	status.TotalOffset = totalOffset

	return status
}

// ==========================================
// 5. HTTP Handlers & Main
// ==========================================

type Server struct {
	cache       *TopologyCache
	kafkaClient *kgo.Client
}

func (s *Server) handleGetTopic(w http.ResponseWriter, r *http.Request) {
	topicName := strings.TrimPrefix(r.URL.Path, "/api/v1/topic/")
	if topicName == "" {
		http.Error(w, "Topic name required", http.StatusBadRequest)
		return
	}

	// 1. Fetch from static in-memory cache
	cachedTopic, exists := s.cache.Get(topicName)
	if !exists {
		http.Error(w, "Topic not found in topology", http.StatusNotFound)
		return
	}

	// 2. Live fetch offsets (with 2s timeout handled internally)
	runtimeStatus := fetchLiveKafkaStatus(s.kafkaClient, topicName)

	// 3. Merge and respond
	response := APIResponse{
		Metadata: cachedTopic.Metadata,
		Topology: cachedTopic.Topology,
		Schema:   cachedTopic.Schema,
		Runtime:  runtimeStatus,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"status":      "UP",
		"topic_count": s.cache.Count(),
		"last_sync":   s.cache.lastUpdate,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Trigger background sync here...
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"status": "refresh triggered"}`))
}

func main() {
	cfg, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize Kafka Client (franz-go)
	kClient, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Kafka.Brokers...),
	)
	if err != nil {
		log.Fatalf("Failed to init Kafka client: %v", err)
	}
	defer kClient.Close()

	server := &Server{
		cache:       NewTopologyCache(),
		kafkaClient: kClient,
	}

	http.HandleFunc("/api/v1/topic/", server.handleGetTopic)
	http.HandleFunc("/api/v1/health", server.handleHealth)
	http.HandleFunc("/api/v1/refresh", server.handleRefresh)

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("Starting Topology Resolver on %s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
