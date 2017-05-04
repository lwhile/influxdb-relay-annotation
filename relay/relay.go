package relay

import (
	"fmt"
	"log"
	"sync"
)

type Service struct {
	relays map[string]Relay
}

type Relay interface {
	Name() string
	Run() error
	Stop() error
}

// construct a Service instant by a config instant
func New(config Config) (*Service, error) {
	s := new(Service)
	s.relays = make(map[string]Relay)

	// 遍历config.HTTPRelays,根据配置实例化服务于HTTP请求的对象
	for _, cfg := range config.HTTPRelays {
		h, err := NewHTTP(cfg)
		if err != nil {
			return nil, err
		}
		// 检查配置文件中的配置outputs列表里是否存在重名.
		// 如果存在重名情况, 停止加载其他配置
		// 这里要注意的是当发生重名的情况后返回给main.go中的调用方后,调用方不会就此终止进程
		// 而是以完成初始化的s.relays对象继续向下运行
		if s.relays[h.Name()] != nil {
			return nil, fmt.Errorf("duplicate relay: %q", h.Name())
		}
		s.relays[h.Name()] = h
	}

	for _, cfg := range config.UDPRelays {
		u, err := NewUDP(cfg)
		if err != nil {
			return nil, err
		}
		if s.relays[u.Name()] != nil {
			return nil, fmt.Errorf("duplicate relay: %q", u.Name())
		}
		s.relays[u.Name()] = u
	}

	return s, nil
}

func (s *Service) Run() {
	var wg sync.WaitGroup
	wg.Add(len(s.relays))

	for k := range s.relays {
		relay := s.relays[k]
		go func() {
			defer wg.Done()

			if err := relay.Run(); err != nil {
				log.Printf("Error running relay %q: %v", relay.Name(), err)
			}
		}()
	}

	wg.Wait()
}

func (s *Service) Stop() {
	for _, v := range s.relays {
		v.Stop()
	}
}
