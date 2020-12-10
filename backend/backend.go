package backend

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/voutilad/bolt-proxy/bolt"
)

type Backend struct {
	monitor      *Monitor
	routingTable *RoutingTable
	tls          bool
	// map of principals -> hosts -> connections
	connectionPool map[string]map[string]bolt.BoltConn
}

func NewBackend(username, password string, uri string, hosts ...string) (*Backend, error) {
	monitor, err := NewMonitor(username, password, uri, hosts...)
	if err != nil {
		return nil, err
	}
	routingTable := <-monitor.C

	tls := false
	switch strings.Split(uri, ":")[0] {
	case "bolt+s", "bolt+ssc", "neo4j+s", "neo4j+ssc":
		tls = true
	default:
	}

	return &Backend{
		monitor:      monitor,
		routingTable: routingTable,
		tls:          tls,
	}, nil
}

func (b *Backend) RoutingTable() *RoutingTable {
	if b.routingTable == nil {
		panic("attempting to use uninitialized BackendClient")
	}

	log.Println("checking routing table...")
	if b.routingTable.Expired() {
		select {
		case rt := <-b.monitor.C:
			b.routingTable = rt
		case <-time.After(60 * time.Second):
			log.Fatal("timeout waiting for new routing table!")
		}
	}

	log.Println("using routing table")
	return b.routingTable
}

// For now, we'll authenticate to all known hosts up-front to simplify things.
// So for a given Hello message, use it to auth against all hosts known in the
// current routing table. Returns an map[string] of hosts to bolt.BoltConn's
// if successful, an empty map and an error if not.
func (b *Backend) Authenticate(hello *bolt.Message) (map[string]bolt.BoltConn, error) {
	if hello.T != bolt.HelloMsg {
		panic("authenticate requires a Hello message")
	}

	// TODO: clean up this api...push the dirt into Bolt package
	msg, pos, err := bolt.ParseTinyMap(hello.Data[4:])
	if err != nil {
		log.Printf("XXX pos: %d, hello map: %#v\n", pos, msg)
		panic(err)
	}
	principal, ok := msg["principal"].(string)
	if !ok {
		panic("principal in Hello message was not a string")
	}
	log.Println("found principal:", principal)

	// refresh routing table
	// TODO: this api seems backwards...push down into table?
	rt := b.RoutingTable()

	// try authing first with the default db writer before we try others
	// this way we can fail fast and not spam a bad set of credentials
	writers, _ := rt.WritersFor(rt.DefaultDb)
	defaultWriter := writers[0]

	log.Printf("trying to auth %s to host %s\n", principal, defaultWriter)
	conn, err := authClient(hello.Data, "tcp", defaultWriter, b.tls)
	if err != nil {
		return nil, err
	}

	// ok, now to get the rest
	conns := make(map[string]bolt.BoltConn, len(rt.Hosts))
	conns[defaultWriter] = bolt.NewDirectConn(conn)

	// we'll need a channel to collect results as we're going async
	type pair struct {
		conn bolt.BoltConn
		host string
	}
	c := make(chan pair, len(rt.Hosts)+1)
	var wg sync.WaitGroup
	for host := range rt.Hosts {
		if host != defaultWriter {
			// done this one already
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := authClient(hello.Data, "tcp", host, b.tls)
				if err != nil {
					log.Printf("failed to auth %s to %s!?\n", principal, host)
					return
				}
				c <- pair{bolt.NewDirectConn(conn), host}
			}()
		}
	}

	wg.Wait()
	close(c)

	// build our connection map
	for p := range c {
		conns[p.host] = p.conn
	}

	log.Printf("auth'd principal to %d hosts\n", len(conns))
	return conns, err
}
