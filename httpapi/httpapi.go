package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/multiformats/go-multiaddr"

	cid "github.com/ipfs/go-cid"
	dsq "github.com/ipfs/go-datastore/query"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-kad-dht/providers"
	"github.com/libp2p/hydra-booster/hydra"
	"github.com/libp2p/hydra-booster/idgen"
)

// ListenAndServe instructs a Hydra HTTP API server to listen and serve on the passed address
func ListenAndServe(hy *hydra.Hydra, addr string) error {
	srv := &http.Server{
		Addr: addr,
		// Good practice to set timeouts to avoid Slowloris attacks.
		WriteTimeout: time.Second * 60,
		ReadTimeout:  time.Second * 60,
		IdleTimeout:  time.Second * 60,
		Handler:      NewRouter(hy),
	}
	return srv.ListenAndServe()
}

// NewRouter creates a new Hydra Booster HTTP API Gorilla Mux
func NewRouter(hy *hydra.Hydra) *mux.Router {
	mux := mux.NewRouter()
	mux.HandleFunc("/heads", headsHandler(hy))
	mux.HandleFunc("/records/fetch/{key}", recordFetchHandler(hy))
	mux.HandleFunc("/records/list", recordListHandler(hy))
	mux.HandleFunc("/idgen/add", idgenAddHandler()).Methods("POST")
	mux.HandleFunc("/idgen/remove", idgenRemoveHandler()).Methods("POST")
	mux.HandleFunc("/swarm/peers", swarmPeersHandler(hy))
	mux.HandleFunc("/pstore/list", pstoreListHandler(hy))
	return mux
}

// "/heads" Get the peers created by hydra booster (ndjson)
func headsHandler(hy *hydra.Hydra) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)

		for _, hd := range hy.Heads {
			enc.Encode(peer.AddrInfo{
				ID:    hd.Host.ID(),
				Addrs: hd.Host.Addrs(),
			})
		}
	}
}

// "/records/fetch" Receive a record and fetch it from the network, if available
func recordFetchHandler(hy *hydra.Hydra) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		cidStr := vars["key"]
		cid, err := cid.Decode(cidStr)
		if err != nil {
			fmt.Printf("Received invalid CID, got %s\n", cidStr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		first := true
		nProviders := 1
		nProvidersStr := r.FormValue("nProviders")
		if nProvidersStr != "" {
			nProviders, err = strconv.Atoi(nProvidersStr)
			if err != nil {
				fmt.Printf("Received invalid nProviders, got %s\n", nProvidersStr)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		enc := json.NewEncoder(w)
		ctx := r.Context()
		for peerAddrInfo := range hy.Heads[0].Routing.FindProvidersAsync(ctx, cid, nProviders) {
			// fmt.Printf("Got one provider %s\n", peerAddrInfo.String())
			// Store the Provider locally
			hy.Heads[0].AddProvider(ctx, cid, peerAddrInfo.ID)
			if first {
				first = false
			}
			enc.Encode(peerAddrInfo)
		}
		if first {
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}
}

// "/records/list" Receive a record and fetch it from the network, if available
func recordListHandler(hy *hydra.Hydra) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		// TODO Improve this handler once ProvideManager gets exposed
		// https://discuss.libp2p.io/t/list-provider-records/450
		// for now, enumerate the Provider Records in the datastore
		ds := hy.SharedDatastore
		results, err := ds.Query(r.Context(), dsq.Query{Prefix: providers.ProvidersKeyPrefix})
		if err != nil {
			fmt.Printf("Error on retrieving provider records: %s\n", err)
			w.WriteHeader(500)
			return
		}

		enc := json.NewEncoder(w)

		for result := range results.Next() {
			enc.Encode(result.Entry)
		}
		results.Close()
	}
}

type PeerInfo struct {
	AddrInfo     peer.AddrInfo // separate field to allow proper (un)marshalling of multi addresses
	HeadID       peer.ID
	AgentVersion string
	Protocols    []string
}

// "/pstore/list" lists all peer information the hydra heads store in their peer stores.
func pstoreListHandler(hy *hydra.Hydra) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		for _, head := range hy.Heads {
			ps := head.Host.Peerstore()
			for _, p := range ps.Peers() {
				// get address information
				pi := ps.PeerInfo(p)

				// get all protocols
				protocols, err := ps.GetProtocols(p)
				if err != nil {
					fmt.Printf("error getting protocols: %s\n", err)
					protocols = []string{}
				}

				// get agent version
				var agentVersion string
				if agent, err := ps.Get(p, "AgentVersion"); err == nil {
					agentVersion = agent.(string)
				}

				// marshal and send response to client
				err = enc.Encode(PeerInfo{
					AddrInfo:     pi,
					HeadID:       head.Host.ID(),
					AgentVersion: agentVersion,
					Protocols:    protocols,
				})
				if err != nil {
					fmt.Printf("error encoding peer info: %s\n", err)
					var netErr net.Error
					if errors.As(err, &netErr) {
						return
					}
				}
			}
		}
	}
}

func idgenAddHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		pk, err := idgen.HydraIdentityGenerator.AddBalanced()
		if err != nil {
			fmt.Println(fmt.Errorf("failed to generate Peer ID: %w", err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		b, err := crypto.MarshalPrivateKey(pk)
		if err != nil {
			fmt.Println(fmt.Errorf("failed to extract private key bytes: %w", err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		enc := json.NewEncoder(w)
		enc.Encode(base64.StdEncoding.EncodeToString(b))
	}
}

func idgenRemoveHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		dec := json.NewDecoder(r.Body)
		var b64 string
		if err := dec.Decode(&b64); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		bytes, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		pk, err := crypto.UnmarshalPrivateKey(bytes)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		err = idgen.HydraIdentityGenerator.Remove(pk)
		if err != nil {
			fmt.Println(fmt.Errorf("failed to remove private key: %w", err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

type swarmPeersPeer struct {
	ID        peer.ID
	Addr      multiaddr.Multiaddr
	Direction network.Direction
}
type swarmPeersHostPeer struct {
	ID   peer.ID
	Peer swarmPeersPeer
}

// "/swarm/peers[?head=]" Get the peers with open connections optionally filtered by hydra head (ndjson)
func swarmPeersHandler(hy *hydra.Hydra) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		headID := r.FormValue("head")

		enc := json.NewEncoder(w)

		for _, hd := range hy.Heads {
			if headID != "" && headID != hd.Host.ID().String() {
				continue
			}

			for _, c := range hd.Host.Network().Conns() {
				enc.Encode(swarmPeersHostPeer{
					ID: hd.Host.ID(),
					Peer: swarmPeersPeer{
						ID:        c.RemotePeer(),
						Addr:      c.RemoteMultiaddr(),
						Direction: c.Stat().Direction,
					},
				})
			}

		}
	}
}
