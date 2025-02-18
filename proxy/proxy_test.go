package proxy

import (
	"context"
	"github.com/DataDog/datadog-go/statsd"
	"github.com/coinbase/redisbetween/config"
	"github.com/coinbase/redisbetween/handlers"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// assumes a redis cluster running with 6 nodes on 127.0.0.1 ports 7000-7005, and
// a standalone redis on port 7006. see docker-compose.yml

func redisHost() string {
	h := os.Getenv("REDIS_HOST")
	if h != "" {
		return h
	}
	return "127.0.0.1"
}

func TestProxy(t *testing.T) {
	sd := setupProxy(t, "7006", -1)

	client := setupStandaloneClient(t, "/var/tmp/redisbetween-"+redisHost()+"-7006.sock")
	res := client.Do(context.Background(), "del", "hello")
	assert.NoError(t, res.Err())
	res = client.Do(context.Background(), "set", "hello", "world")
	assert.NoError(t, res.Err())
	res = client.Do(context.Background(), "get", "hello")
	assert.NoError(t, res.Err())
	assert.Equal(t, "get hello: world", res.String())
	err := client.Close()
	assert.NoError(t, err)
	sd()
}

type command struct {
	cmd  string
	args []string
	res  string
}

func TestIntegrationCommands(t *testing.T) {
	shutdownProxy := setupProxy(t, "7000", -1)
	clusterClient := setupClusterClient(t, "/var/tmp/redisbetween-"+redisHost()+"-7000.sock")
	var i int
	var wg sync.WaitGroup
	for {
		go func(index int, t *testing.T) {
			var j int
			ind := strconv.Itoa(index)
			for {
				j++
				if j == 20 {
					wg.Done()
					break
				}
				s := ind + strconv.Itoa(j)
				assertResponse(t, command{cmd: "set", args: []string{s, "hi"}, res: "set " + s + " hi: OK"}, clusterClient)
				assertResponse(t, command{cmd: "get", args: []string{s}, res: "get " + s + ": hi"}, clusterClient)
			}
		}(i, t)
		wg.Add(1)
		i++
		if i == 10 {
			break
		}
	}
	wg.Wait()
	shutdownProxy()
}

func TestPipelinedCommands(t *testing.T) {
	shutdownProxy := setupProxy(t, "7006", 3)
	client := setupStandaloneClient(t, "/var/tmp/redisbetween-"+redisHost()+"-7006-3.sock")
	var i int
	var wg sync.WaitGroup
	for {
		go func(index int, t *testing.T) {
			var j int
			ind := strconv.Itoa(index)
			commands := []command{{cmd: "get", args: []string{string(handlers.PipelineSignalStartKey)}, res: "get 🔜: redis: nil"}}
			for {
				j++
				if j == 20 {
					break
				}
				s := ind + strconv.Itoa(j)
				commands = append(commands, command{cmd: "set", args: []string{s, "hi"}, res: "set " + s + " hi: OK"})
				commands = append(commands, command{cmd: "get", args: []string{s}, res: "get " + s + ": hi"})
			}
			commands = append(commands, command{cmd: "get", args: []string{string(handlers.PipelineSignalEndKey)}, res: "get 🔚: redis: nil"})
			assertResponsePipelined(t, commands, client)
			wg.Done()
		}(i, t)
		wg.Add(1)
		i++
		if i == 10 {
			break
		}
	}
	wg.Wait()
	shutdownProxy()
}

func TestDbSelectCommand(t *testing.T) {
	shutdown := setupProxy(t, "7006", 3)
	client := setupStandaloneClient(t, "/var/tmp/redisbetween-"+redisHost()+"-7006-3.sock")
	res := client.Do(context.Background(), "CLIENT", "LIST")
	assert.NoError(t, res.Err())
	assert.Contains(t, res.String(), "db=3")
	shutdown()
}

func TestLocalSocketPathFromUpstream(t *testing.T) {
	assert.Equal(t, "prefix-with.host-colon.suffix", localSocketPathFromUpstream("with.host:colon", -1, "prefix-", ".suffix"))
	assert.Equal(t, "prefix-withoutcolon.host.suffix", localSocketPathFromUpstream("withoutcolon.host", -1, "prefix-", ".suffix"))
	assert.Equal(t, "prefix-with.host-db-1.suffix", localSocketPathFromUpstream("with.host:db", 1, "prefix-", ".suffix"))
}

func assertResponse(t *testing.T, cmd command, c *redis.ClusterClient) {
	args := make([]interface{}, len(cmd.args)+1)
	args[0] = cmd.cmd
	for i, a := range cmd.args {
		args[i+1] = a
	}
	res := c.Do(context.Background(), args...)
	assert.Equal(t, cmd.res, res.String())
}

func assertResponsePipelined(t *testing.T, cmds []command, c *redis.Client) {
	p := c.Pipeline()
	actuals := make([]*redis.Cmd, len(cmds))
	expected := make([]string, len(cmds))
	for i, cmd := range cmds {
		args := make([]interface{}, len(cmd.args)+1)
		args[0] = cmd.cmd
		for i, a := range cmd.args {
			args[i+1] = a
		}
		actuals[i] = p.Do(context.Background(), args...)
		expected[i] = cmd.res
	}
	_, _ = p.Exec(context.Background())
	actualStrings := make([]string, len(actuals))
	for i, a := range actuals {
		actualStrings[i] = a.String()
	}
	assert.Equal(t, expected, actualStrings)
}

func setupProxy(t *testing.T, upstreamPort string, db int) func() {
	t.Helper()

	uri := redisHost() + ":" + upstreamPort

	sd, err := statsd.New("localhost:8125")
	assert.NoError(t, err)

	cfg := &config.Config{
		Network:           "unix",
		LocalSocketPrefix: "/var/tmp/redisbetween-",
		LocalSocketSuffix: ".sock",
		Unlink:            true,
	}

	proxy, err := NewProxy(zap.L(), sd, cfg, "test", uri, db, 1, 1, 1*time.Second, 1*time.Second)
	assert.NoError(t, err)
	go func() {
		err := proxy.Run()
		assert.NoError(t, err)
	}()

	time.Sleep(1 * time.Second) // todo find a more elegant way to do this

	return func() {
		proxy.Shutdown()
	}
}

func setupStandaloneClient(t *testing.T, address string) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Network: "unix", Addr: address, MaxRetries: 1})
	res := client.Do(context.Background(), "ping")
	if res.Err() != nil {
		_ = client.Close()
		// Use t.Fatalf instead of assert because we want to fail fast if the cluster is down.
		t.Fatalf("error pinging redis: %v", res.Err())
	}
	return client
}

func setupClusterClient(t *testing.T, address string) *redis.ClusterClient {
	t.Helper()
	opt := &redis.ClusterOptions{
		Addrs: []string{address},
		Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// redis client patch that translates tcp connection attempts to the local socket instead
			if strings.Contains(network, "tcp") {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				addr = "/var/tmp/redisbetween-" + host + "-" + port + ".sock"
				network = "unix"
			}
			return net.Dial(network, addr)
		},
		MaxRetries: 1,
	}
	client := redis.NewClusterClient(opt)
	res := client.Do(context.Background(), "ping")
	if res.Err() != nil {
		_ = client.Close()
		// Use t.Fatalf instead of assert because we want to fail fast if the cluster is down.
		t.Fatalf("error pinging redis: %v", res.Err())
	}
	return client
}
