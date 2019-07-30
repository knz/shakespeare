package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"hash/fnv"
	"html"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Result struct {
	// shakespeare version used to produce the result.
	Version string

	// Play title
	Title string

	// Play author(s)
	Authors string

	// "See also" strings.
	SeeAlso []string

	// Whether a foul was detected.
	Foul bool

	// The rendered error object.
	Error string

	// The timestamp of the start of the experiment in standard format.
	Timestamp string

	// The timestamp in human format.
	TimestampHTML string

	// Duration in seconds.
	PlayDuration float64

	// Duration represented as string.
	PlayDurationVerbose string

	// Time boundaries
	MinTime float64
	MaxTime float64

	// The full config string.
	Config string

	// The config hash (FNV 32-bit).
	ConfigHash uint32

	// The config hash in a human-friendly form (easy to read aloud).
	ConfigHashHTML string

	// The prettty-printed config string.
	ConfigHTML string

	// The compiled script.
	Steps string

	// The pretty-printed compiled script.
	StepsHTML string

	// Repeat section if any.
	Repeat *RepeatSection

	// Artifacts.
	Artifacts []Artifact

	// Diffs.
	Diffs []string
}

type RepeatSection struct {
	StartTime float64

	Duration float64

	FirstRepeatedAct int

	LastRepeatedAct int

	NumRepeats int
}

type Artifact struct {
	// Note: the JSON tags are chosen to match the requirements
	// from jsTree.
	FileName    string `json:"text"`
	Path        string
	Icon        string `json:"icon"`
	IsDir       bool
	ContentType string
	Children    []Artifact `json:"children,omitEmpty"`
}

func (ap *app) assemble(ctx context.Context, foundErr error) *Result {
	// Sanity checking.
	if math.IsInf(ap.maxTime, 0) || math.IsInf(ap.minTime, 0) {
		ap.expandTimeRange(0)
	}

	if ap.maxTime < ap.minTime {
		ap.minTime, ap.maxTime = ap.maxTime, ap.minTime
	}

	playDuration := time.Duration(float64(time.Second) * (ap.maxTime - ap.minTime))
	// Round to the lower second hundredth.
	playDuration = (playDuration / (time.Second / 100)) * (time.Second / 100)

	// Ensure the x axis always start at zero, even if no
	// event was received until later on the time line.
	if ap.minTime > 0 {
		ap.minTime = 0
	}
	// Sanity check.
	if ap.maxTime < 0 {
		ap.maxTime = 1
	}
	// More sanity check.
	if ap.maxTime < ap.minTime+1 {
		ap.maxTime = ap.minTime + 1
	}

	if !ap.cfg.avoidTimeProgress {
		ap.narrate(I, "ℹ️ ", "the timeline extends from %.2fs to %.2fs, relative to %s",
			ap.minTime, ap.maxTime, ap.epoch())
	}

	r := &Result{
		Version:             versionName,
		Title:               joinAnd(ap.cfg.titleStrings),
		Authors:             joinAnd(ap.cfg.authors),
		SeeAlso:             ap.cfg.seeAlso,
		Foul:                foundErr != nil,
		Timestamp:           ap.epoch().Format(time.RFC3339),
		TimestampHTML:       formatDatePretty(ap.epoch()),
		MinTime:             ap.minTime,
		MaxTime:             ap.maxTime,
		PlayDuration:        playDuration.Seconds(),
		PlayDurationVerbose: playDuration.String(),
		Diffs:               make([]string, 0, len(ap.cfg.diffs)),
	}

	var buf bytes.Buffer
	ap.cfg.printCfg(&buf, true /*skipComs*/, true /*skipVer*/, false /*annot*/)
	r.Config = buf.String()
	h := fnv.New32()
	h.Write(buf.Bytes())
	r.ConfigHash = h.Sum32()
	r.ConfigHashHTML = GenName(int64(r.ConfigHash))

	buf.Reset()
	ap.cfg.printCfg(&buf, true /*skipComs*/, true /*skipVer*/, true /*annot*/)
	r.ConfigHTML = buf.String()
	buf.Reset()
	ap.cfg.printSteps(&buf, false /*annot*/)
	r.Steps = buf.String()
	buf.Reset()
	ap.cfg.printSteps(&buf, true /*annot*/)
	r.StepsHTML = buf.String()

	if foundErr != nil {
		buf.Reset()
		RenderError(&buf, foundErr)
		r.Error = buf.String()
	}

	repeatTs := math.Inf(-1)
	beforeLastTs := repeatTs
	if ap.cfg.repeatActNum > 0 {
		// Find the timestamp where the last repetition started.
		for _, acn := range ap.auRes.actChanges {
			if acn.actNum == ap.cfg.repeatActNum {
				beforeLastTs = repeatTs
				repeatTs = acn.ts
				// No break here: we want to get the ts for the last occurrence.
			}
		}
	}
	if !math.IsInf(beforeLastTs, 0) {
		// Use the next-to-last iteration if available.
		repeatTs = beforeLastTs
	}
	hasRepeat := !math.IsInf(repeatTs, 0)
	if hasRepeat {
		rp := &RepeatSection{
			StartTime:        repeatTs,
			Duration:         ap.maxTime - repeatTs,
			FirstRepeatedAct: ap.cfg.repeatActNum,
			LastRepeatedAct:  len(ap.cfg.play),
			NumRepeats:       ap.auRes.numRepeats,
		}

		r.Repeat = rp
	}

	for _, d := range ap.cfg.diffs {
		r.Diffs = append(r.Diffs, html.EscapeString(d))
	}

	return r
}

func (ap *app) collectArtifacts(r *Result) {
	a := ap.collectArtifactsRec(ap.cfg.dataDir)
	r.Artifacts = append(r.Artifacts, a.Children...)
}

func (ap *app) collectArtifactsRec(dir string) Artifact {
	a := Artifact{
		FileName: filepath.Base(dir),
		IsDir:    true,
		Icon:     "📁",
	}
	a.Path, _ = filepath.Rel(ap.cfg.dataDir, dir)
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		name := filepath.Base(path)
		if (strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#")) || strings.HasSuffix(name, "~") {
			// Editor temp files. Ignore.
			return nil
		}
		var subA Artifact
		if info.IsDir() {
			subA = ap.collectArtifactsRec(path)
			if len(subA.Children) > 0 {
				a.Children = append(a.Children, subA)
			}
			return filepath.SkipDir
		} else {
			subA.FileName = name
			subA.Path, _ = filepath.Rel(ap.cfg.dataDir, path)
			subA.ContentType, subA.Icon = detectType(path)
			a.Children = append(a.Children, subA)
		}

		return nil
	})
	return a
}

func detectType(path string) (string, string) {
	idx := strings.LastIndex(path, ".")
	if idx >= 0 {
		ext := path[idx+1:]
		switch ext {
		case "svg":
			return "image/xml+svg", "🖼️"
		case "csv":
			return "text/csv", "📈"
		case "md":
			return "text/markdown", "📝"
		case "html":
			return "text/html", "📄"
		case "pdf":
			return "application/pdf", "📄"
		case "log", "txt":
			return "text/plain", "📄"
		case "gp", "sh":
			return "text/plain", "📜"
		case "js":
			return "application/javascript", "📜"
		}
	}
	return "application/octet-stream", "👾"
}

func (ap *app) writeResult(ctx context.Context, res *Result) error {
	const jsonFile = "result.js"
	fName := filepath.Join(ap.cfg.dataDir, jsonFile)
	res.Artifacts = append(res.Artifacts, Artifact{
		FileName:    jsonFile,
		Path:        jsonFile,
		ContentType: "application/javascript",
		Icon:        "📜",
	})
	j, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}

	f, err := os.Create(fName)
	if err != nil {
		return err
	}
	defer f.Close()
	if !ap.cfg.avoidTimeProgress {
		ap.narrate(I, "📜", "result JSON: %s", fName)
	}
	f.WriteString("var result = ")
	_, err = f.Write(j)
	return err
}
