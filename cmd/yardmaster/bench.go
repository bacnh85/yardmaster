package main

import (
	"flag"
	"os"

	"github.com/bacnh85/yardmaster/bench"
)

func cmdBench(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	conns := fs.Int("conns", 100, "concurrent streams")
	rate := fs.Int("rate", 300, "tokens/sec per stream")
	toks := fs.Int("toks", 300, "tokens per stream")
	ttft := fs.Int("ttft", 120, "upstream TTFT (ms)")
	fs.Parse(args)

	res, err := bench.Run(bench.Options{
		Streams: *conns, Rate: *rate, Toks: *toks, TTFTms: *ttft,
	})
	if err != nil {
		fatal(err)
	}
	res.Print(os.Stdout)
}
