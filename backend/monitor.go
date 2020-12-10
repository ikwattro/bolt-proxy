package backend

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v4/neo4j"
)

// Modeled after time.Ticker, a Monitor will keep tabs on the Neo4j routing
// table behind the scenes. It auto-adjusts the refresh interval to match
// the server's declared TTL recommendation.
//
// As it creates new RoutingTable instances on the heap, it will put pointers
// to new instances into the channel C. (Similar to how time.Ticker puts the
// current time into its channel.)
//
// Known issue: if the channel isn't read, new values drop. This meas the value
// could be stale and needs to be checked.
type Monitor struct {
	C      <-chan *RoutingTable
	halt   chan bool
	driver *neo4j.Driver
}

// Our default Driver configuration provides:
// - custom user-agent name
// - ability to add in specific list of hosts to use for address resolution
func newConfigurer(hosts []string) func(c *neo4j.Config) {
	return func(c *neo4j.Config) {
		c.AddressResolver = func(addr neo4j.ServerAddress) []neo4j.ServerAddress {
			if len(hosts) == 0 {
				return []neo4j.ServerAddress{addr}
			}

			addrs := make([]neo4j.ServerAddress, len(hosts))
			for i, host := range hosts {
				parts := strings.Split(host, ":")
				if len(parts) != 2 {
					panic(fmt.Sprintf("invalid host: %s", host))
				}
				addrs[i] = neo4j.NewServerAddress(parts[0], parts[1])
			}
			return addrs
		}
		c.UserAgent = "bolt-proxy/v0"
	}
}

func NewMonitor(user, password, uri string, hosts ...string) (*Monitor, error) {
	c := make(chan *RoutingTable, 1)
	h := make(chan bool, 1)

	// Try immediately to connect to Neo4j
	auth := neo4j.BasicAuth(user, password, "")
	driver, err := neo4j.NewDriver(uri, auth, newConfigurer(hosts))
	if err != nil {
		return nil, err
	}

	// Get the first routing table and ttl details
	rt, err := getNewRoutingTable(&driver)
	if err != nil {
		log.Fatal(err)
	}
	c <- rt

	monitor := Monitor{c, h, &driver}
	go func() {
		// preset the initial ticker to use the first ttl measurement
		ticker := time.NewTicker(rt.Ttl)
		for {
			select {
			case <-ticker.C:
				rt, err := getNewRoutingTable(monitor.driver)
				if err != nil {
					log.Fatal(err)
				}
				ticker.Reset(rt.Ttl)

				// empty the channel and put the new value in
				// this looks odd, but even though it's racy,
				// it should be racy in a safe way since it
				// doesn't matter if another go routine takes
				// the value first
				select {
				case <-c:
				default:
				}
				select {
				case c <- rt:
				default:
					panic("monitor channel full")
				}
			case <-h:
				ticker.Stop()
				log.Println("monitor stopped")
			case <-time.After(5 * rt.Ttl):
				log.Fatalf("monitor timeout reached of 5 x %v\n", rt.Ttl)
			}
		}
	}()

	return &monitor, nil
}

func (m *Monitor) Stop() {
	select {
	case (*m).halt <- true:
	default:
	}
}

// local data structure for passing the raw routing table details
type table struct {
	db      string
	ttl     time.Duration
	readers []string
	writers []string
}

// Denormalize the routing table
const ROUTING_QUERY = `
UNWIND $names AS name
CALL dbms.routing.getRoutingTable({}, name)
  YIELD ttl, servers
WITH name, ttl, servers
UNWIND servers AS server
WITH name, ttl, server
UNWIND server["addresses"] AS address
RETURN name, ttl, server["role"] AS role, address
`

// Dump the list of databases with the first result being the default db
const SHOW_DATABASES = `
SHOW DATABASES YIELD name, default, currentStatus WHERE currentStatus = 'online'
RETURN name, default, currentStatus ORDER BY default DESC
`

// Use SHOW DATABASES to dump the current list of databases with the first
// database name being the default (based on the query logic)
func queryDbNames(driver *neo4j.Driver) ([]string, error) {
	session := (*driver).NewSession(neo4j.SessionConfig{
		DatabaseName: "system",
	})
	defer session.Close()

	result, err := session.Run(SHOW_DATABASES, nil)
	if err != nil {
		return nil, err
	}
	rows, err := result.Collect()
	if err != nil {
		return nil, err
	}

	names := make([]string, len(rows))
	for i, row := range rows {
		val, found := row.Get("name")
		if !found {
			return nil, errors.New("couldn't find name field in result")
		}
		name, ok := val.(string)
		if !ok {
			panic("name isn't a string")
		}
		names[i] = name
	}

	return names, nil
}

func queryRoutingTable(driver *neo4j.Driver, names []string) (map[string]table, error) {
	session := (*driver).NewSession(neo4j.SessionConfig{})
	defer session.Close()

	result, err := session.ReadTransaction(func(tx neo4j.Transaction) (interface{}, error) {
		return routingTableTx(tx, names)
	})
	if err != nil {
		return map[string]table{}, err
	}

	tableMap, ok := result.(map[string]table)
	if !ok {
		return map[string]table{}, errors.New("invalid type for routing table response")
	}

	return tableMap, nil
}

func routingTableTx(tx neo4j.Transaction, names []string) (interface{}, error) {
	params := make(map[string]interface{}, 1)
	params["names"] = names
	result, err := tx.Run(ROUTING_QUERY, params)
	if err != nil {
		return nil, err
	}

	rows, err := result.Collect()
	if err != nil {
		return nil, err
	}

	// expected fields: [name, ttl, role, address]
	tableMap := make(map[string]table, len(rows))
	for _, row := range rows {
		val, found := row.Get("address")
		if !found {
			return nil, errors.New("missing address field in result")
		}
		addr, ok := val.(string)
		if !ok {
			panic("addr isn't a string!")
		}

		val, found = row.Get("ttl")
		if !found {
			return nil, errors.New("missing ttl field in result")
		}
		ttl, ok := val.(int64)
		if !ok {
			panic("ttl isn't an integer!")
		}

		val, found = row.Get("name")
		if !found {
			return nil, errors.New("missing name field in result")
		}
		name, ok := val.(string)
		if !ok {
			panic("name isn't a string!")
		}

		t, found := tableMap[name]
		if !found {
			t = table{
				db:  name,
				ttl: time.Duration(ttl) * time.Second,
			}
		}

		val, found = row.Get("role")
		if !found {
			return nil, errors.New("missing role field in result")
		}
		role, ok := val.(string)
		if !ok {
			panic("role isn't a string")
		}

		switch role {
		case "READ":
			t.readers = append(t.readers, addr)
		case "WRITE":
			t.writers = append(t.writers, addr)
		case "ROUTE":
			continue
		default:
			return nil, errors.New("invalid role")
		}

		tableMap[name] = t
	}

	return tableMap, nil
}

func getNewRoutingTable(driver *neo4j.Driver) (*RoutingTable, error) {
	names, err := queryDbNames(driver)
	if err != nil {
		log.Printf("error getting database names: %v\n", err)
		return nil, err
	}

	tableMap, err := queryRoutingTable(driver, names)
	if err != nil {
		log.Printf("error getting routing table: %v\n", err)
		return nil, err
	}

	// build the new routing table instance
	// TODO: clean this up...seems smelly..
	readers := make(map[string][]string)
	writers := make(map[string][]string)
	rt := RoutingTable{
		DefaultDb: names[0],
		readers:   readers,
		writers:   writers,
		CreatedAt: time.Now(),
		Hosts:     make(map[string]bool),
	}
	for db, t := range tableMap {
		r := make([]string, len(t.readers))
		copy(r, t.readers)
		w := make([]string, len(t.writers))
		copy(w, t.writers)
		rt.readers[db] = r
		rt.writers[db] = w

		// yes, this is redundant...
		rt.Ttl = t.ttl

		// yes, this is also wasteful...construct host sets
		for _, host := range r {
			rt.Hosts[host] = true
		}
		for _, host := range w {
			rt.Hosts[host] = true
		}
	}

	log.Printf("updated routing table: %s\n", &rt)
	log.Printf("known hosts look like: %s\n", rt.Hosts)

	return &rt, nil
}
