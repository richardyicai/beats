package elasticsearch

import (
	"errors"
	"fmt"
	"sync"

	"github.com/elastic/beats/libbeat/beat"
	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/common/transport/tlscommon"
	"github.com/elastic/beats/libbeat/logp"
	"github.com/elastic/beats/libbeat/outputs"
	"github.com/elastic/beats/libbeat/outputs/outil"
)

func init() {
	outputs.RegisterType("elasticsearch", makeES)
}

var (
	debugf = logp.MakeDebug("elasticsearch")
)

var (
	// ErrNotConnected indicates failure due to client having no valid connection
	ErrNotConnected = errors.New("not connected")

	// ErrJSONEncodeFailed indicates encoding failures
	ErrJSONEncodeFailed = errors.New("json encode failed")

	// ErrResponseRead indicates error parsing Elasticsearch response
	ErrResponseRead = errors.New("bulk item status parse failed")
)

type callbacksRegistry struct {
	callbacks []connectCallback
	mutex     sync.Mutex
}

// XXX: it would be fantastic to do this without a package global
var connectCallbackRegistry callbacksRegistry

// RegisterConnectCallback registers a callback for the elasticsearch output
// The callback is called each time the client connects to elasticsearch.
func RegisterConnectCallback(callback connectCallback) {
	connectCallbackRegistry.mutex.Lock()
	defer connectCallbackRegistry.mutex.Unlock()
	connectCallbackRegistry.callbacks = append(connectCallbackRegistry.callbacks, callback)
}

func makeES(
	beat beat.Info,
	observer outputs.Observer,
	cfg *common.Config,
) (outputs.Group, error) {
	if !cfg.HasField("bulk_max_size") {
		cfg.SetInt("bulk_max_size", -1, defaultBulkSize)
	}

	if !cfg.HasField("index") {
		pattern := fmt.Sprintf("%v-%v-%%{+yyyy.MM.dd}", beat.IndexPrefix, beat.Version)
		cfg.SetString("index", -1, pattern)
	}

	config := defaultConfig
	if err := cfg.Unpack(&config); err != nil {
		return outputs.Fail(err)
	}

	hosts, err := outputs.ReadHostList(cfg)
	if err != nil {
		return outputs.Fail(err)
	}

	index, err := outil.BuildSelectorFromConfig(cfg, outil.Settings{
		Key:              "index",
		MultiKey:         "indices",
		EnableSingleOnly: true,
		FailEmpty:        true,
	})
	if err != nil {
		return outputs.Fail(err)
	}

	tlsConfig, err := tlscommon.LoadTLSConfig(config.TLS)
	if err != nil {
		return outputs.Fail(err)
	}

	pipelineSel, err := outil.BuildSelectorFromConfig(cfg, outil.Settings{
		Key:              "pipeline",
		MultiKey:         "pipelines",
		EnableSingleOnly: true,
		FailEmpty:        false,
	})
	if err != nil {
		return outputs.Fail(err)
	}

	var pipeline *outil.Selector
	if !pipelineSel.IsEmpty() {
		pipeline = &pipelineSel
	}

	proxyURL, err := parseProxyURL(config.ProxyURL)
	if err != nil {
		return outputs.Fail(err)
	}
	if proxyURL != nil {
		logp.Info("Using proxy URL: %s", proxyURL)
	}

	params := config.Params
	if len(params) == 0 {
		params = nil
	}

	clients := make([]outputs.NetworkClient, len(hosts))
	for i, host := range hosts {
		esURL, err := common.MakeURL(config.Protocol, config.Path, host, 9200)
		if err != nil {
			logp.Err("Invalid host param set: %s, Error: %v", host, err)
			return outputs.Fail(err)
		}

		var client outputs.NetworkClient
		client, err = NewClient(ClientSettings{
			URL:              esURL,
			Index:            index,
			Pipeline:         pipeline,
			Proxy:            proxyURL,
			TLS:              tlsConfig,
			Username:         config.Username,
			Password:         config.Password,
			Parameters:       params,
			Headers:          config.Headers,
			Timeout:          config.Timeout,
			CompressionLevel: config.CompressionLevel,
			Observer:         observer,
		}, &connectCallbackRegistry)
		if err != nil {
			return outputs.Fail(err)
		}

		client = outputs.WithBackoff(client, config.Backoff.Init, config.Backoff.Max)
		clients[i] = client
	}

	return outputs.SuccessNet(config.LoadBalance, config.BulkMaxSize, config.MaxRetries, clients)
}

// NewConnectedClient creates a new Elasticsearch client based on the given config.
// It uses the NewElasticsearchClients to create a list of clients then returns
// the first from the list that successfully connects.
func NewConnectedClient(cfg *common.Config) (*Client, error) {
	clients, err := NewElasticsearchClients(cfg)
	if err != nil {
		return nil, err
	}

	errors := []string{}

	for _, client := range clients {
		err = client.Connect()
		if err != nil {
			logp.Err("Error connecting to Elasticsearch at %v: %v", client.Connection.URL, err)
			err = fmt.Errorf("Error connection to Elasticsearch %v: %v", client.Connection.URL, err)
			errors = append(errors, err.Error())
			continue
		}
		return &client, nil
	}
	return nil, fmt.Errorf("Couldn't connect to any of the configured Elasticsearch hosts. Errors: %v", errors)
}

// NewElasticsearchClients returns a list of Elasticsearch clients based on the given
// configuration. It accepts the same configuration parameters as the output,
// except for the output specific configuration options (index, pipeline,
// template) .If multiple hosts are defined in the configuration, a client is returned
// for each of them.
func NewElasticsearchClients(cfg *common.Config) ([]Client, error) {
	hosts, err := outputs.ReadHostList(cfg)
	if err != nil {
		return nil, err
	}

	config := defaultConfig
	if err = cfg.Unpack(&config); err != nil {
		return nil, err
	}

	tlsConfig, err := tlscommon.LoadTLSConfig(config.TLS)
	if err != nil {
		return nil, err
	}

	proxyURL, err := parseProxyURL(config.ProxyURL)
	if err != nil {
		return nil, err
	}
	if proxyURL != nil {
		logp.Info("Using proxy URL: %s", proxyURL)
	}

	params := config.Params
	if len(params) == 0 {
		params = nil
	}

	clients := []Client{}
	for _, host := range hosts {
		esURL, err := common.MakeURL(config.Protocol, config.Path, host, 9200)
		if err != nil {
			logp.Err("Invalid host param set: %s, Error: %v", host, err)
			return nil, err
		}

		client, err := NewClient(ClientSettings{
			URL:              esURL,
			Proxy:            proxyURL,
			TLS:              tlsConfig,
			Username:         config.Username,
			Password:         config.Password,
			Parameters:       params,
			Headers:          config.Headers,
			Timeout:          config.Timeout,
			CompressionLevel: config.CompressionLevel,
		}, nil)
		if err != nil {
			return clients, err
		}
		clients = append(clients, *client)
	}
	if len(clients) == 0 {
		return clients, fmt.Errorf("No hosts defined in the Elasticsearch output")
	}
	return clients, nil
}
