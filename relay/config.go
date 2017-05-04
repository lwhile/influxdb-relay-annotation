package relay

import (
	"os"

	"github.com/naoina/toml"
)

// Config abstract config file
type Config struct {
	// Reference to sample.toml could find two major config item: HTTP and UDP
	// 参考sample.toml会发现配置项分为两大类: HTTP 和 UDP
	HTTPRelays []HTTPConfig `toml:"http"`
	UDPRelays  []UDPConfig  `toml:"udp"`
}

// HTTPConfig abstract http config
type HTTPConfig struct {
	// Name identifies the HTTP relay
	Name string `toml:"name"`

	// Addr should be set to the desired listening host:port
	Addr string `toml:"bind-addr"`

	// Set certificate in order to handle HTTPS requests
	SSLCombinedPem string `toml:"ssl-combined-pem"`

	// Default retention policy to set for forwarded requests
	// 请求转发到influxdb之前可以写入配置好的数据保存策略
	DefaultRetentionPolicy string `toml:"default-retention-policy"`

	// Outputs is a list of backed servers where writes will be forwarded
	Outputs []HTTPOutputConfig `toml:"output"`
}

type HTTPOutputConfig struct {
	// Name of the backend server
	Name string `toml:"name"`

	// Location should be set to the URL of the backend server's write endpoint
	Location string `toml:"location"`

	// Timeout sets a per-backend timeout for write requests. (Default 10s)
	// The format used is the same seen in time.ParseDuration
	Timeout string `toml:"timeout"`

	// Buffer failed writes up to maximum count. (Default 0, retry/buffering disabled)
	BufferSizeMB int `toml:"buffer-size-mb"`

	// Maximum batch size in KB (Default 512)
	MaxBatchKB int `toml:"max-batch-kb"`

	// Maximum delay between retry attempts.
	// The format used is the same seen in time.ParseDuration (Default 10s)
	MaxDelayInterval string `toml:"max-delay-interval"`

	// Skip TLS verification in order to use self signed certificate.
	// WARNING: It's insecure. Use it only for developing and don't use in production.
	// todo: ?
	SkipTLSVerification bool `toml:"skip-tls-verification"`
}

type UDPConfig struct {
	// Name identifies the UDP relay
	Name string `toml:"name"`

	// Addr is where the UDP relay will listen for packets
	Addr string `toml:"bind-addr"`

	// Precision sets the precision of the timestamps (input and output)
	Precision string `toml:"precision"`

	// ReadBuffer sets the socket buffer for incoming connections
	ReadBuffer int `toml:"read-buffer"`

	// Outputs is a list of backend servers where writes will be forwarded
	Outputs []UDPOutputConfig `toml:"output"`
}

type UDPOutputConfig struct {
	// Name identifies the UDP backend
	Name string `toml:"name"`

	// Location should be set to the host:port of the backend server
	Location string `toml:"location"`

	// MTU sets the maximum output payload size, default is 1024
	MTU int `toml:"mtu"`
}

// LoadConfigFile parses the specified file into a Config object
// 配置文件的载入放在config相关文件,可以避免在main.go加入了文件的读写逻辑
func LoadConfigFile(filename string) (cfg Config, err error) {
	f, err := os.Open(filename)
	if err != nil {
		return cfg, err
	}
	defer f.Close()

	// good tasty.
	return cfg, toml.NewDecoder(f).Decode(&cfg)
}
