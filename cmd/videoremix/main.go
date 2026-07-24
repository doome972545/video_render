// Command videoremix is the CLI front end. All pipeline logic lives in
// pkg/app; this file only parses flags and drives the reusable core, so the
// exact same core can be embedded in a Wails desktop app or an HTTP server.
package main

import (
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"videoremix/pkg/app"
)

func main() {
	var (
		source      = flag.String("source", "", "source URL or local file path")
		count       = flag.Int("variants", 5, "number of variants to generate")
		seed        = flag.Int64("seed", 0, "master seed for reproducibility (0 = time-based)")
		workDir     = flag.String("workdir", "work", "working directory for downloads")
		outDir      = flag.String("out", "output", "output directory for rendered videos")
		gpu         = flag.Bool("gpu", false, "prefer GPU (nvenc) encoding when available")
		concurrency = flag.Int("concurrency", 4, "max parallel renders")
	)
	flag.Parse()

	if *source == "" {
		log.Fatal("--source is required (URL or local file path)")
	}

	svc, err := app.New(app.Config{
		WorkDir:     *workDir,
		OutputDir:   *outDir,
		PreferGPU:   *gpu,
		Concurrency: *concurrency,
		OnLog:       func(line string) { fmt.Println("  " + line) },
	})
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	defer svc.Close()

	fmt.Printf("Starting job for %q (variants=%d)\n", *source, *count)
	id, err := svc.StartJob(app.JobRequest{
		Source:       *source,
		VariantCount: *count,
		Seed:         *seed,
		Priority:     app.PriorityNormal,
	})
	if err != nil {
		log.Fatalf("start job: %v", err)
	}
	fmt.Printf("Job started: %s\n", id)

	// Poll and print status until the job reaches a terminal phase.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		st, err := svc.Status(id)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("[%s] phase=%s %d/%d done (%.0f%%)\n",
			time.Now().Format("15:04:05"), st.Phase,
			st.Completed+st.Failed+st.Cancelled, st.Total, st.Percent)

		if st.Terminal() {
			if st.Error != "" {
				log.Fatalf("job ended in %s: %s", st.Phase, st.Error)
			}
			fmt.Printf("Job %s finished: %s. Outputs in %s\n", id, st.Phase, filepath.Clean(*outDir))
			return
		}
		<-ticker.C
	}
}
