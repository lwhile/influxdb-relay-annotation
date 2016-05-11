# InfluxDB Relay

This project adds a basic high availability layer to InfluxDB. With the right architecture and disaster recovery processes, this achieves a highly available setup. It will be available with the 0.12.0 release of InfluxDB in April 2016.

## Usage

To build from source and run:

```sh
$ # Install influxdb-relay to your $GOPATH/bin
$ go install github.com/influxdata/influxdb-relay
$ # Edit your configuration file
$ cp $GOPATH/github.com/influxdata/influxdb-relay/sample.toml ./relay.toml
$ vim relay.toml
$ # Start relay!
$ $GOPATH/influxdb-relay -config relay.toml
```

## Configuration

```toml
[[http]]
# Name of the HTTP server, used for display purposes only.
name = "example-http"

# TCP address to bind to, for HTTP server.
bind-addr = "127.0.0.1:9096"

# Array of InfluxDB instances to use as backends for Relay.
output = [
    # name: name of the backend, used for display purposes only.
    # location: full URL of the /write endpoint of the backend
    # timeout: Go-parseable time duration. Fail writes if incomplete in this time.
    { name="local1", location="http://127.0.0.1:8086/write", timeout="10s" },
    { name="local2", location="http://127.0.0.1:7086/write", timeout="10s" },
]

[[udp]]
# Name of the UDP server, used for display purposes only.
name = "example-udp"

# UDP address to bind to.
bind-addr = "127.0.0.1:9096"

# Socket buffer size for incoming connections.
read-buffer = 0 # default

# Precision to use for timestamps
precision = "n" # Can be n, u, ms, s, m, h

# Array of InfluxDB instances to use as backends for Relay.
output = [
    # name: name of the backend, used for display purposes only.
    # location: host and port of backend.
    # mtu: maximum output payload size
    { name="local1", location="127.0.0.1:8089", mtu=512 },
    { name="local2", location="127.0.0.1:7089", mtu=1024 },
]
```

## Description

The architecture is fairly simple and consists of a load balancer, two or more InfluxDB Relay processes and two or more InfluxDB processes. The load balancer should point UDP traffic and HTTP POST requests with the path `/write` to the two relays while pointing GET requests with the path `/query` to the two InfluxDB servers.

The setup should look like this:

```
        ┌─────────────────┐                 
        │writes & queries │                 
        └─────────────────┘                 
                 │                          
                 ▼                          
         ┌───────────────┐                  
         │               │                  
┌────────│ Load Balancer │─────────┐        
│        │               │         │        
│        └──────┬─┬──────┘         │        
│               │ │                │        
│               │ │                │        
│        ┌──────┘ └────────┐       │        
│        │ ┌─────────────┐ │       │┌──────┐
│        │ │/write or UDP│ │       ││/query│
│        ▼ └─────────────┘ ▼       │└──────┘
│  ┌──────────┐      ┌──────────┐  │        
│  │ InfluxDB │      │ InfluxDB │  │        
│  │ Relay    │      │ Relay    │  │        
│  └──┬────┬──┘      └────┬──┬──┘  │        
│     │    |              |  │     │        
│     |  ┌─┼──────────────┘  |     │        
│     │  │ └──────────────┐  │     │        
│     ▼  ▼                ▼  ▼     │        
│  ┌──────────┐      ┌──────────┐  │        
│  │          │      │          │  │        
└─▶│ InfluxDB │      │ InfluxDB │◀─┘        
   │          │      │          │           
   └──────────┘      └──────────┘           
 ```


The relay will listen for HTTP or UDP writes and write the data to each InfluxDB server via the HTTP write or UDP endpoint, as appropriate. If the write is sent via HTTP, the relay will return a success response as soon as one of the InfluxDB servers returns a success. If any InfluxDB server returns a 400 response, that will be returned to the client immediately. If all servers return a 500, a 500 will be returned to the client. If some but not all servers return a 500 that will not be returned to the client. You can monitor each instance's logs for 500 errors.

With this setup a failure of one Relay or one InfluxDB can be sustained while still taking writes and serving queries. However, the recovery process will require operator intervention.

## Recovery

InfluxDB organizes its data on disk into logical blocks of time called shards. We can use this to create a hot recovery process with zero downtime.

The length of time that shards represent in InfluxDB are typically 1 hour, 1 day, or 7 days, depending on the retention duration, but can be explicitly set when creating the retention policy. For the sake of our example, let's assume shard durations of 1 day.

Let's say one of the InfluxDB servers goes down for an hour on 2016-03-10. Once midnight UTC rolls over, all InfluxDB processes are now writing data to the shard for 2016-03-11 and the file(s) for 2016-03-10 have gone cold for writes. We can then restore things using these steps:

1. Tell the load balancer to stop sending query traffic to the server that was down (this should be done as soon as an outage is detected to prevent partial or inconsistent query returns.)
2. Create backup of 2016-03-10 shard from a server that was up the entire day
3. Restore the backup of the shard from the good server to the server that had downtime
4. Tell the load balancer to resume sending queries to the previously downed server

During this entire process the Relays should be sending current writes to all servers, including the one with downtime.

## Sharding

It's possible to add another layer on top of this kind of setup to shard data. Depending on your needs you could shard on the measurement name or a specific tag like `customer_id`. The sharding layer would have to service both queries and writes.
