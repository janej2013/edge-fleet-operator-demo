// firmware-server: the "vendor CDN" of the demo. http.FileServer speaks
// Range requests natively, which is what makes the agent's resumable
// downloads work with zero extra code here.
package main

import (
	"flag"
	"log"
	"net/http"
)

func main() {
	dir := flag.String("dir", ".", "directory of firmware images")
	addr := flag.String("addr", ":8000", "listen address")
	flag.Parse()
	log.Printf("serving firmware from %s on %s", *dir, *addr)
	log.Fatal(http.ListenAndServe(*addr, http.FileServer(http.Dir(*dir))))
}
