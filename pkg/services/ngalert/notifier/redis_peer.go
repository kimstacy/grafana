package notifier

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/google/uuid"
	"github.com/prometheus/alertmanager/cluster"
	"github.com/prometheus/alertmanager/cluster/clusterpb"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/redis/go-redis/v9"

	"github.com/grafana/grafana/pkg/infra/log"
)

type redisConfig struct {
	addr     string
	username string
	password string
	db       int
	name     string
	prefix   string
}

const (
	peerPattern          = "*"
	fullState            = "full_state"
	fullStateChannel     = fullState
	fullStateChannelReq  = fullStateChannel + ":request"
	update               = "update"
	redisServerLabel     = "redis-server"
	networkRetryInterval = time.Second * 10
)

type redisPeer struct {
	name   string
	redis  *redis.Client
	prefix string
	logger log.Logger
	states map[string]cluster.State
	subs   map[string]*redis.PubSub
	mtx    sync.RWMutex

	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration

	readyc    chan struct{}
	shutdownc chan struct{}

	pushPullInterval time.Duration

	messagesReceived     *prometheus.CounterVec
	messagesReceivedSize *prometheus.CounterVec
	messagesSent         *prometheus.CounterVec
	messagesSentSize     *prometheus.CounterVec
	nodePingDuration     *prometheus.HistogramVec

	// Last known position in the cluster.
	position int
	// The time when we computed the position in the cluster the last time successfully.
	positionFetchedAt time.Time
	// The duration we want to return the position if the network is down.
	positionValidFor time.Duration
}

func newRedisPeer(cfg redisConfig, logger log.Logger, reg prometheus.Registerer,
	pushPullInterval time.Duration) (*redisPeer, error) {
	name := "peer-" + uuid.New().String()
	// If a specific name is provided, overwrite default one.
	if cfg.name != "" {
		name = cfg.name
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.addr,
		Username: cfg.username,
		Password: cfg.password,
		DB:       cfg.db,
	})
	cmd := rdb.Ping(context.Background())
	if cmd.Err() != nil {
		return nil, fmt.Errorf("failed to ping redis: %w", cmd.Err())
	}
	// Make sure that the prefix uses a colon at the end as deliminator.
	if cfg.prefix != "" && cfg.prefix[len(cfg.prefix)-1] != ':' {
		cfg.prefix = cfg.prefix + ":"
	}
	p := &redisPeer{
		name:              name,
		redis:             rdb,
		logger:            logger,
		states:            map[string]cluster.State{},
		subs:              map[string]*redis.PubSub{},
		pushPullInterval:  pushPullInterval,
		readyc:            make(chan struct{}),
		shutdownc:         make(chan struct{}),
		prefix:            cfg.prefix,
		heartbeatInterval: time.Second * 5,
		heartbeatTimeout:  time.Minute,
		positionValidFor:  time.Minute,
	}

	// The metrics for the redis peer are exactly the same as for the official
	// upstream Memberlist implementation. Three metrics that doesn't make sense
	// for redis are not available: messagesPruned, messagesQueued, nodeAlive.
	messagesReceived := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "alertmanager_cluster_messages_received_total",
		Help: "Total number of cluster messages received.",
	}, []string{"msg_type"})
	messagesReceivedSize := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "alertmanager_cluster_messages_received_size_total",
		Help: "Total size of cluster messages received.",
	}, []string{"msg_type"})
	messagesSent := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "alertmanager_cluster_messages_sent_total",
		Help: "Total number of cluster messages sent.",
	}, []string{"msg_type"})
	messagesSentSize := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "alertmanager_cluster_messages_sent_size_total",
		Help: "Total size of cluster messages sent.",
	}, []string{"msg_type"})
	gossipClusterMembers := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "alertmanager_cluster_members",
		Help: "Number indicating current number of members in cluster.",
	}, func() float64 {
		return float64(p.ClusterSize())
	})
	peerPosition := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "alertmanager_peer_position",
		Help: "Position the Alertmanager instance believes it's in. The position determines a peer's behavior in the cluster.",
	}, func() float64 {
		return float64(p.Position())
	})
	healthScore := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "alertmanager_cluster_health_score",
		Help: "Health score of the cluster. Lower values are better and zero means 'totally healthy'.",
	}, func() float64 {
		return float64(p.GetHealthScore())
	})
	nodePingDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "alertmanager_cluster_pings_seconds",
		Help:    "Histogram of latencies for ping messages.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5},
	}, []string{"peer"},
	)

	messagesReceived.WithLabelValues(fullState)
	messagesReceivedSize.WithLabelValues(fullState)
	messagesReceived.WithLabelValues(update)
	messagesReceivedSize.WithLabelValues(update)
	messagesSent.WithLabelValues(fullState)
	messagesSentSize.WithLabelValues(fullState)
	messagesSent.WithLabelValues(update)
	messagesSentSize.WithLabelValues(update)

	reg.MustRegister(messagesReceived, messagesReceivedSize, messagesSent, messagesSentSize,
		gossipClusterMembers, peerPosition, healthScore, nodePingDuration,
	)

	p.messagesReceived = messagesReceived
	p.messagesReceivedSize = messagesReceivedSize
	p.messagesSent = messagesSent
	p.messagesSentSize = messagesSentSize
	p.nodePingDuration = nodePingDuration

	p.subs[fullStateChannel] = p.redis.Subscribe(context.Background(), p.withPrefix(fullStateChannel))
	p.subs[fullStateChannelReq] = p.redis.Subscribe(context.Background(), p.withPrefix(fullStateChannelReq))

	go p.heartbeatLoop()
	go p.fullStateSyncPublishLoop()
	go p.fullStateSyncReceiveLoop()
	go p.fullStateReqReceiveLoop()

	return p, nil
}

func (p *redisPeer) withPrefix(str string) string {
	return p.prefix + str
}

func (p *redisPeer) heartbeatLoop() {
	ticker := time.NewTicker(p.heartbeatInterval)
	for {
		select {
		case <-ticker.C:
			startTime := time.Now()
			cmd := p.redis.Set(context.Background(), p.withPrefix(p.name), time.Now().Unix(), time.Minute*5)
			reqDur := time.Since(startTime)
			if cmd.Err() != nil {
				p.logger.Error("error setting the heartbeat key", "err", cmd.Err(), "peer", p.withPrefix(p.name))
				continue
			}
			p.nodePingDuration.WithLabelValues(redisServerLabel).Observe(reqDur.Seconds())
		case <-p.shutdownc:
			ticker.Stop()
			return
		}
	}
}

func (p *redisPeer) Position() int {
	members := p.Members()
	// When the members array is empty and we fetched our position successfully
	// prior, we can assume that currently the network is down.
	// To prevent a spike of duplicate messages, we return for the duration of
	// lastPositionValidFor the last known position and only failover to position
	// 0 afterwards if we do not eventually recover.
	if len(members) == 0 && p.positionFetchedAt.After(time.Now().Add(-p.positionValidFor)) {
		p.logger.Warn("failed to fetch position from redis, falling back to last know position", "last_known", p.position)
		return p.position
	}
	for i, peer := range members {
		if peer == p.name {
			p.logger.Debug("cluster position found", "name", p.name, "position", i)
			p.position = i
			p.positionFetchedAt = time.Now()
			return i
		}
	}

	return 0
}

// Returns the known size of the Cluster. This also includes dead nodes that
// haven't timeout yet.
func (p *redisPeer) ClusterSize() int {
	scan := p.redis.Scan(context.Background(), 0, p.withPrefix(peerPattern), 100)
	if scan.Err() != nil {
		p.logger.Error("error getting keys from redis", "err", scan.Err(), "pattern", p.withPrefix(peerPattern))
		return 0
	}
	members, _ := scan.Val()
	return len(members)
}

// If the cluster is healthy it should return 0, otherwise the number of
// unhealthy nodes.
func (p *redisPeer) GetHealthScore() int {
	size := p.ClusterSize()
	members := len(p.Members())
	if size > members {
		return size - members
	}
	return 0
}

// Members returns a list of active cluster Members.
func (p *redisPeer) Members() []string {
	// The 100 is a hint for the server, how many records there might be for the
	// provided pattern. It _might_ only return the first 100 records, which should
	// be more than enough for our use case.
	// More here: https://redis.io/commands/scan/
	members, _, err := p.redis.Scan(context.Background(), 0, p.withPrefix(peerPattern), 100).Result()
	if err != nil {
		p.logger.Error("error getting keys from redis", "err", err, "pattern", p.withPrefix(peerPattern))
		return []string{}
	}
	// This might happen on startup, when no value is in the store yet.
	if len(members) == 0 {
		return []string{}
	}
	values := p.redis.MGet(context.Background(), members...)
	if values.Err() != nil {
		p.logger.Error("error getting values from redis", "err", values.Err(), "keys", members)
	}
	peers := []string{}
	// After getting the list of possible members from redis, we filter
	// those out that have failed to send a heartbeat during the heartbeatTimeout.
	for i, peer := range members {
		val := values.Val()[i]
		if val == nil {
			continue
		}
		ts, err := strconv.ParseInt(val.(string), 10, 64)
		if err != nil {
			panic(err)
		}
		tm := time.Unix(ts, 0)
		if tm.Before(time.Now().Add(-p.heartbeatTimeout)) {
			continue
		}
		peers = append(peers, peer)
	}
	sort.Strings(peers)
	return peers
}

func (p *redisPeer) WaitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.readyc:
		return nil
	}
}

// Settle is mostly copied from uptream.
// Ref: https://github.com/prometheus/alertmanager/blob/2888649b473970400c0bd375fdd563486dc80481/cluster/cluster.go#L674-L712
func (p *redisPeer) Settle(ctx context.Context, interval time.Duration) {
	const NumOkayRequired = 3
	p.logger.Info("Waiting for gossip to settle...", "interval", interval)
	start := time.Now()
	nPeers := 0
	nOkay := 0
	totalPolls := 0
	for {
		select {
		case <-ctx.Done():
			elapsed := time.Since(start)
			p.logger.Info("gossip not settled but continuing anyway", "polls", totalPolls, "elapsed", elapsed)
			close(p.readyc)
			return
		case <-time.After(interval):
		}
		elapsed := time.Since(start)
		n := len(p.Members())
		if nOkay >= NumOkayRequired {
			p.logger.Info("gossip settled; proceeding", "elapsed", elapsed)
			break
		}
		if n == nPeers {
			nOkay++
			p.logger.Debug("gossip looks settled", "elapsed", elapsed)
		} else {
			nOkay = 0
			p.logger.Info("gossip not settled", "polls", totalPolls, "before", nPeers, "now", n, "elapsed", elapsed)
		}
		nPeers = n
		totalPolls++
	}
	p.requestFullState()
	close(p.readyc)
}

func (p *redisPeer) AddState(key string, state cluster.State, _ prometheus.Registerer) cluster.ClusterChannel {
	p.mtx.Lock()
	p.states[key] = state
	// As we also want to get the state from other nodes, we subscribe to the key.
	sub := p.redis.Subscribe(context.Background(), p.withPrefix(key))
	go p.receiveLoop(key, sub)
	p.subs[key] = sub
	p.mtx.Unlock()
	return &RedisChannel{
		p:       p,
		channel: p.withPrefix(key),
		msgType: update,
	}
}

func (p *redisPeer) receiveLoop(name string, channel *redis.PubSub) {
	for {
		select {
		case <-p.shutdownc:
			return
		default:
			data, err := channel.ReceiveMessage(context.Background())
			var opErr *net.OpError
			if errors.As(err, &opErr) {
				p.logger.Error("network error, waiting 10 seconds before retry", "err", err, "channel", p.withPrefix(name))
				time.Sleep(networkRetryInterval)
				continue
			}
			if err != nil {
				p.logger.Error("error receiving message from redis", "err", err, "channel", p.withPrefix(name))
				continue
			}
			p.messagesReceived.WithLabelValues(update).Inc()
			p.messagesReceivedSize.WithLabelValues(update).Add(float64(len(data.Payload)))
			var part clusterpb.Part
			if err := proto.Unmarshal([]byte(data.Payload), &part); err != nil {
				p.logger.Warn("error decoding the received broadcast message", "err", err)
				continue
			}

			p.mtx.RLock()
			s, ok := p.states[part.Key]
			p.mtx.RUnlock()

			if !ok {
				continue
			}
			if err := s.Merge(part.Data); err != nil {
				p.logger.Warn("error merging the received broadcast message", "err", err, "key", name)
				continue
			}
		}
	}
}

func (p *redisPeer) fullStateReqReceiveLoop() {
	for {
		select {
		case <-p.shutdownc:
			return
		default:
			data, err := p.subs[fullStateChannelReq].ReceiveMessage(context.Background())
			var opErr *net.OpError
			if errors.As(err, &opErr) {
				p.logger.Error("network error, waiting 10 seconds before retry", "err", err, "channel", p.withPrefix(fullStateChannelReq))
				time.Sleep(networkRetryInterval)
				continue
			}
			if err != nil {
				p.logger.Error("error receiving message from redis", "err", err, "channel", p.withPrefix(fullStateChannelReq))
				continue
			}
			// The payload of a full state request is the name of the peer that is
			// requesting the full state. In case we received our own request, we
			// can just ignore it. Redis pub/sub fanouts to all clients, regardless
			// if a client was also the publisher.
			if data.Payload == p.name {
				continue
			}
			p.fullStateSyncPublish()
		}
	}
}

func (p *redisPeer) fullStateSyncReceiveLoop() {
	for {
		select {
		case <-p.shutdownc:
			return
		default:
			data, err := p.subs[fullStateChannel].ReceiveMessage(context.Background())
			var opErr *net.OpError
			if errors.As(err, &opErr) {
				p.logger.Error("network error, waiting 10 seconds before retry", "err", err, "channel", p.withPrefix(fullStateChannel))
				time.Sleep(networkRetryInterval)
				continue
			}
			if err != nil {
				p.logger.Error("error receiving message from redis", "err", err, "channel", p.withPrefix(fullStateChannel))
				continue
			}
			p.messagesReceived.WithLabelValues(fullState).Inc()
			p.messagesReceivedSize.WithLabelValues(fullState).Add(float64(len(data.Payload)))

			var fs clusterpb.FullState
			if err := proto.Unmarshal([]byte(data.Payload), &fs); err != nil {
				p.logger.Warn("error unmarshaling the received remote state", "err", err)
				continue
			}
			// This inline func is just a lazy workaround so we can use defer in the loop.
			func() {
				p.mtx.RLock()
				defer p.mtx.RUnlock()
				for _, part := range fs.Parts {
					s, ok := p.states[part.Key]
					if !ok {
						p.logger.Warn("received", "unknown state key", "len", len(data.Payload), "key", part.Key)
						continue
					}
					if err := s.Merge(part.Data); err != nil {
						p.logger.Warn("error merging the received remote state", "err", err, "key", part.Key)
						return
					}
				}
			}()
		}
	}
}

func (p *redisPeer) fullStateSyncPublish() {
	pub := p.redis.Publish(context.Background(), p.withPrefix(fullStateChannel), p.LocalState())
	if pub.Err() != nil {
		p.logger.Error("error publishing a message to redis", "err", pub.Err(), "channel", p.withPrefix(fullStateChannel))
	}
}

func (p *redisPeer) fullStateSyncPublishLoop() {
	ticker := time.NewTicker(p.pushPullInterval)
	for {
		select {
		case <-ticker.C:
			p.fullStateSyncPublish()
		case <-p.shutdownc:
			ticker.Stop()
			return
		}
	}
}

func (p *redisPeer) requestFullState() {
	pub := p.redis.Publish(context.Background(), p.withPrefix(fullStateChannelReq), p.name)
	if pub.Err() != nil {
		p.logger.Error("error publishing a message to redis", "err", pub.Err(), "channel", p.withPrefix(fullStateChannelReq))
	}
}

func (p *redisPeer) LocalState() []byte {
	p.mtx.RLock()
	defer p.mtx.RUnlock()
	all := &clusterpb.FullState{
		Parts: make([]clusterpb.Part, 0, len(p.states)),
	}

	for key, s := range p.states {
		b, err := s.MarshalBinary()
		if err != nil {
			p.logger.Warn("error encoding the local state", "err", err, "key", key)
		}
		all.Parts = append(all.Parts, clusterpb.Part{Key: key, Data: b})
	}
	b, err := proto.Marshal(all)
	if err != nil {
		p.logger.Warn("error encoding the local state to proto", "err", err)
	}
	p.messagesSent.WithLabelValues(fullState).Inc()
	p.messagesSentSize.WithLabelValues(fullState).Add(float64(len(b)))
	return b
}

func (p *redisPeer) Shutdown() {
	p.logger.Info("Stopping redis peer...")
	close(p.shutdownc)
	p.fullStateSyncPublish()
	del := p.redis.Del(context.Background(), p.withPrefix(p.name))
	if del.Err() != nil {
		p.logger.Error("error deleting the redis key on shutdown", "err", del.Err(), "key", p.withPrefix(p.name))
	}
}

type RedisChannel struct {
	p       *redisPeer
	channel string
	msgType string
}

func (c *RedisChannel) Broadcast(b []byte) {
	b, err := proto.Marshal(&clusterpb.Part{Key: c.channel, Data: b})
	if err != nil {
		return
	}
	pub := c.p.redis.Publish(context.Background(), c.channel, string(b))
	// An error here might not be as critical as one might think on first sight.
	// The state will eventually be propagted to other members by the full sync.
	if pub.Err() != nil {
		c.p.logger.Error("error publishing a message to redis", "err", pub.Err(), "channel", c.channel)
		return
	}
	c.p.messagesSent.WithLabelValues(c.msgType).Inc()
	c.p.messagesSentSize.WithLabelValues(c.msgType).Add(float64(len(b)))
}
