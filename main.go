package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"sync"
	"time"

	human "github.com/dustin/go-humanize"
	crypto "github.com/libp2p/go-libp2p-crypto"
	inet "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	pstore "github.com/libp2p/go-libp2p-peerstore"
	swarm "github.com/libp2p/go-libp2p-swarm"
	disc "github.com/libp2p/go-libp2p/p2p/discovery"
	bhost "github.com/libp2p/go-libp2p/p2p/host/basic"
	ma "github.com/multiformats/go-multiaddr"
	cli "github.com/urfave/cli"
	ui "github.com/whyrusleeping/gooey"
)

type notifee struct {
	h *bhost.BasicHost
}

func (n *notifee) HandlePeerFound(pi pstore.PeerInfo) {
	n.h.Connect(context.Background(), pi)
}

func makeHost() (*bhost.BasicHost, error) {
	// Generate an identity keypair using go's cryptographic randomness source
	priv, pub, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		panic(err)
	}

	// A peers ID is the hash of its public key
	pid, err := peer.IDFromPublicKey(pub)
	if err != nil {
		panic(err)
	}

	// We've created the identity, now we need to store it.
	// A peerstore holds information about peers, including your own
	ps := pstore.NewPeerstore()
	ps.AddPrivKey(pid, priv)
	ps.AddPubKey(pid, pub)

	maddr, err := ma.NewMultiaddr("/ip4/0.0.0.0/tcp/0")
	if err != nil {
		panic(err)
	}

	// Make a context to govern the lifespan of the swarm
	ctx := context.Background()

	// Put all this together
	netw, err := swarm.NewNetwork(ctx, []ma.Multiaddr{maddr}, pid, ps, nil)
	if err != nil {
		panic(err)
	}

	h, err := bhost.New(netw)
	if err != nil {
		return nil, err
	}

	svc, err := disc.NewMdnsService(ctx, h, time.Second*5)
	if err != nil {
		return nil, err
	}

	svc.RegisterNotifee(&notifee{h})

	return h, nil
}

func main() {
	c := cli.NewApp()
	c.Commands = []cli.Command{
		sendCommand,
		recvCommand,
	}
	c.RunAndExitOnError()
}

type hello struct {
	Name     string
	Hostname string
	File     string
	Size     uint64
	peer     peer.ID
}

var sendCommand = cli.Command{
	Name: "send",
	Action: func(c *cli.Context) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		h, err := makeHost()
		if err != nil {
			return err
		}

		finame := c.Args().First()

		name, err := os.Hostname()
		if err != nil {
			return err
		}

		u, err := user.Current()
		if err != nil {
			return err
		}

		st, err := os.Stat(finame)
		if err != nil {
			return err
		}

		app := new(ui.App)
		app.Title = "p2pdrop"
		app.Log = ui.NewLog(3, 10)

		myhello := hello{
			Name:     u.Username,
			Hostname: name,
			File:     finame,
			Size:     uint64(st.Size()),
		}
		h.Network().SetConnHandler(func(c inet.Conn) {
			s, err := h.NewStream(ctx, c.RemotePeer(), "/p2pdrop/1.0.0/hello")
			if err != nil {
				app.Log.Add(fmt.Sprintf("error opening stream: %s", err))
				return
			}
			defer s.Close()

			if err := json.NewEncoder(s).Encode(myhello); err != nil {
				app.Log.Add(fmt.Sprintf("error writing hello: %s", err))
				return
			}
		})

		h.SetStreamHandler("/p2pdrop/1.0.0/hello", func(s inet.Stream) {
			defer s.Close()
			var otherhello hello
			if err := json.NewDecoder(s).Decode(&otherhello); err != nil {
				app.Log.Add(fmt.Sprintf("reading hello: %s", err))
				return
			}

			app.Log.Add(fmt.Sprintf("Found someone: %s@%s - %s (%s)", otherhello.Name, otherhello.Hostname, otherhello.File, human.Bytes(otherhello.Size)))
		})
		h.SetStreamHandler("/p2pdrop/1.0.0/get", func(s inet.Stream) {
			defer s.Close()
			fi, err := os.Open(finame)
			if err != nil {
				fmt.Println("error opening file: ", err)
				return
			}
			_, err = io.Copy(s, fi)
			if err != nil {
				fmt.Println("error copying file: ", err)
				return
			}
		})

		for range time.Tick(time.Second) {
			app.Print()
		}
		return nil
	},
}

var recvCommand = cli.Command{
	Name: "recv",
	Action: func(c *cli.Context) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		h, err := makeHost()
		if err != nil {
			return err
		}

		name, err := os.Hostname()
		if err != nil {
			return err
		}

		u, err := user.Current()
		if err != nil {
			return err
		}

		app := new(ui.App)
		app.Title = "p2pdrop"
		app.Log = ui.NewLog(3, 10)

		myhello := hello{
			Name:     u.Username,
			Hostname: name,
		}

		h.Network().SetConnHandler(func(c inet.Conn) {
			s, err := h.NewStream(ctx, c.RemotePeer(), "/p2pdrop/1.0.0/hello")
			if err != nil {
				app.Log.Add(fmt.Sprintf("error opening stream: %s", err))
				return
			}
			defer s.Close()

			if err := json.NewEncoder(s).Encode(myhello); err != nil {
				app.Log.Add(fmt.Sprintf("error writing hello: %s", err))
				return
			}
		})

		var hellolk sync.Mutex
		var hellos []hello
		h.SetStreamHandler("/p2pdrop/1.0.0/hello", func(s inet.Stream) {
			defer s.Close()
			var otherhello hello
			if err := json.NewDecoder(s).Decode(&otherhello); err != nil {
				app.Log.Add(fmt.Sprintf("reading hello: %s", err))
				return
			}
			if otherhello.File == "" {
				return
			}

			otherhello.peer = s.Conn().RemotePeer()

			hellolk.Lock()
			n := len(hellos)
			hellos = append(hellos, otherhello)
			hellolk.Unlock()

			app.Log.Add(fmt.Sprintf("%d: %s@%s - %s (%s)", n, otherhello.Name, otherhello.Hostname, otherhello.File, human.Bytes(otherhello.Size)))
		})

		app.NewDataLine(13, "Select file by number:", "")
		app.NewDataLine(2, "-------", "")
		go func() {
			for range time.Tick(time.Second) {
				app.Print()
			}
		}()

		scan := bufio.NewScanner(os.Stdin)
		for scan.Scan() {
			n, err := strconv.Atoi(scan.Text())
			if err != nil {
				app.Log.Add(fmt.Sprintf("input error: %s", err))
				continue
			}

			hellolk.Lock()
			hl := hellos[n]
			hellolk.Unlock()

			fmt.Printf("fetching %s from %s\n", hl.File, hl.Name)
			s, err := h.NewStream(ctx, hl.peer, "/p2pdrop/1.0.0/get")
			if err != nil {
				fmt.Println("Errr:", err)
				break
			}

			outfi, err := os.Create(hl.File)
			if err != nil {
				fmt.Println("create err: ", err)
				break
			}
			defer outfi.Close()

			_, err = io.Copy(outfi, s)
			if err != nil {
				fmt.Println("create err: ", err)
				break
			}
			fmt.Println("Success!")
			break
		}
		return nil
	},
}
