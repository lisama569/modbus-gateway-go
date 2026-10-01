// Package gateway 在本地模拟从站之上组装 Modbus 接入网关:
// 统一帧模型、会话级串行客户端与点位批量读取。
package gateway

import (
	"time"

	"github.com/lisama569/modbus-gateway-go/pkg/mapping"
	"github.com/lisama569/modbus-gateway-go/pkg/modbus"
)

// Config 描述一套本地网关:一个模拟从站 + 若干映射点位 + 客户端参数。
type Config struct {
	Transport modbus.Transport
	Unit      byte
	SlaveSize int
	Timeout   time.Duration
	Retry     int
	Logger    modbus.Logger
	Points    []mapping.Point
}

// Gateway 是本地 Modbus 接入网关句柄。
type Gateway struct {
	server *modbus.Server
	client *modbus.Client
	slave  *modbus.Slave
	points []mapping.Point
	writer *mapping.Writer
}

// Start 启动本地模拟从站并连接会话客户端。
// slave 为可选(传入 nil 时由网关新建 65536 地址的从站)。
func Start(cfg Config, slave *modbus.Slave) (*Gateway, error) {
	if slave == nil {
		size := cfg.SlaveSize
		if size <= 0 {
			size = 65536
		}
		slave = modbus.NewSlave(size)
	}
	server, err := modbus.ServeLocal(cfg.Transport, slave)
	if err != nil {
		return nil, err
	}
	client, err := modbus.Dial(modbus.ClientConfig{
		Transport: cfg.Transport,
		Addr:      server.Addr(),
		Unit:      cfg.Unit,
		Timeout:   cfg.Timeout,
		Retry:     cfg.Retry,
		Logger:    cfg.Logger,
	})
	if err != nil {
		_ = server.Close()
		return nil, err
	}
	return &Gateway{
		server: server,
		client: client,
		slave:  slave,
		points: cfg.Points,
		writer: mapping.NewWriter(client),
	}, nil
}

// Slave 返回底层模拟从站,供测试预置数据与注入故障。
func (g *Gateway) Slave() *modbus.Slave { return g.slave }

// ReadAll 对全部已配置点位执行一次批量读取。
func (g *Gateway) ReadAll() (*mapping.BatchResult, error) {
	return mapping.ReadBatch(g.client, g.points)
}

// ReadPoints 对指定点位执行一次批量读取。
func (g *Gateway) ReadPoints(points []mapping.Point) (*mapping.BatchResult, error) {
	return mapping.ReadBatch(g.client, points)
}

// WriteAll 对全部已配置点位按 (point, value) 批量写回。
// values 的键为点位名称;未提供值的点位按单项失败处理。
func (g *Gateway) WriteAll(values map[string]any, cfg mapping.WriteConfig) (*mapping.WriteBatchResult, error) {
	reqs := make([]mapping.WriteRequest, 0, len(g.points))
	for _, p := range g.points {
		reqs = append(reqs, mapping.WriteRequest{Point: p, Value: values[p.Name]})
	}
	return g.writer.WriteBatch(reqs, cfg)
}

// WriteRequests 对显式给出的写回请求执行批量写回。
func (g *Gateway) WriteRequests(reqs []mapping.WriteRequest, cfg mapping.WriteConfig) (*mapping.WriteBatchResult, error) {
	return g.writer.WriteBatch(reqs, cfg)
}

// Client 暴露底层客户端,供直接按功能码访问。
func (g *Gateway) Client() *modbus.Client { return g.client }

// Close 关闭客户端与本地服务端。
func (g *Gateway) Close() error {
	cerr := g.client.Close()
	serr := g.server.Close()
	if cerr != nil {
		return cerr
	}
	return serr
}
