package main

import (
	_ "github.com/coredns/coredns/core/plugin"
	_ "github.com/hyandell/coredns-multicluster"

	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/coremain"
)

func init() {
	var added bool

	// insert multicluster plugin after kubernetes
	for i, name := range dnsserver.Directives {
		if name == "kubernetes" {
			dnsserver.Directives = append(dnsserver.Directives[:i],
				append([]string{"multicluster"}, dnsserver.Directives[i:]...)...)
			added = true
			break
		}
	}

	if !added {
		panic("plugin multicluster not added to dnsserver.Directives")
	}
}

func main() {
	coremain.Run()
}
