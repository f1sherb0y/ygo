// hpserver is the ygo server in Hocuspocus mode with the hooks the
// end-to-end test (../hocuspocus-provider.mjs) needs: the token "bad" is
// refused with the reason "bad-token", every other token is accepted, and
// documents whose name starts with "ro-" are read-only.
package main

import (
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/Deln0r/ygo/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	auth := flag.Bool("auth", true, "check tokens")
	flag.Parse()
	opts := server.Options{
		Hocuspocus:     true,
		OriginPatterns: []string{"*"},
		ReadOnly:       func(docName string, _ *http.Request) bool { return strings.HasPrefix(docName, "ro-") },
	}
	if *auth {
		opts.OnAuthenticate = func(_, token string) error {
			if token == "bad" {
				return errors.New("bad-token")
			}
			return nil
		}
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s", listener.Addr())
	log.Fatal(http.Serve(listener, server.New(opts).Handler()))
}
