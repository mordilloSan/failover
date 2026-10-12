package agent

import (
	"cmp"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The mirror, watched once an hour from the tick: whether the TOS backup
// still writes to it (its btrfs generation), and its folders, so a new one
// or a changed compose is told the same day, not the day it would matter.

const mirrorEvery = time.Hour

// watchMirror runs from the tick, under mu; it is a few stats and one btrfs
// call, and only once an hour.
func (a *Agent) watchMirror() {
	if !a.mirrorAt.IsZero() && a.now.Sub(a.mirrorAt) < mirrorEvery {
		return
	}
	a.mirrorAt = a.now
	a.mirrorGeneration()
	a.mirrorFolders()
}

// mirrorGeneration asks btrfs which files had data written since the last
// look (find-new lists only those: a snapshot of the mirror or an access
// time moves the subvolume's generation but writes no data). None for
// mirror_stale_days means the backup stopped, and a failover would start
// from data that old.
func (a *Agent) mirrorGeneration() {
	st := &a.st
	since := st.MirrorGen
	if since == 0 {
		since = 1 << 62 // the first look: only the marker, not every file of the mirror
	}
	out, err := a.sys.Output("btrfs", "subvolume", "find-new", a.cfg.Paths.MirrorSubvol, strconv.FormatInt(since, 10))
	if err != nil {
		return // not a btrfs subvolume here (the e2e): nothing to tell
	}
	var marker int64
	written := false
	for line := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "transid marker was "); ok {
			marker, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		} else if strings.TrimSpace(line) != "" {
			written = true
		}
	}
	first := st.MirrorGen == 0
	if marker > 0 {
		st.MirrorGen = marker
	}
	if first || written {
		if st.MirrorStale {
			a.event("", "o espelho voltou a mudar: o backup do TOS está outra vez a correr")
		}
		st.MirrorChanged, st.MirrorStale = a.now, false
		return
	}
	days := cmp.Or(a.cfg.Paths.MirrorStaleDays, 2)
	if !st.MirrorStale && a.now.Sub(st.MirrorChanged) >= time.Duration(days)*24*time.Hour {
		st.MirrorStale = true
		a.alert("", fmt.Sprintf("o espelho não muda desde %s: o backup do TOS parou? Um failover arrancaria com os dados desse dia", st.MirrorChanged.Format("02/01 15:04")))
	}
}

// mirrorFolders compares the folders with a docker-compose.yml, and when
// each one changed, with what it saw an hour ago.
func (a *Agent) mirrorFolders() {
	c, st := &a.cfg, &a.st
	root := filepath.Join(c.Paths.MirrorSubvol, c.Paths.MirrorRoot)
	if _, err := os.Stat(root); err != nil {
		return // not there (being mounted, a wrong path): the settings' checks say so
	}
	now := map[string]int64{}
	for _, d := range append(mirrorDirs(root, c.NPM.Dir), c.NPM.Dir) {
		if slices.Contains(c.Ignored, d) {
			continue
		}
		if fi, err := os.Stat(filepath.Join(root, d, "docker-compose.yml")); err == nil {
			now[d] = fi.ModTime().Unix()
		}
	}
	owner := map[string]string{c.NPM.Dir: "npm"}
	for _, sv := range c.Services {
		owner[sv.Dir] = sv.Name
	}
	if st.MirrorSeen == nil { // the first look: what is there is known, not new
		st.MirrorSeen = now
		return
	}
	var changed bool
	for _, d := range slices.Sorted(maps.Keys(now)) {
		old, seen := st.MirrorSeen[d]
		name := owner[d]
		switch {
		case !seen && name == "":
			st.MirrorNew = append(st.MirrorNew, d)
			a.alert("", "pasta nova no espelho: "+d+", por proteger (Definições → Serviços)")
		case seen && old != now[d] && name != "":
			changed = true
			a.alert(name, "o docker-compose.yml mudou no espelho: as imagens e o que o TNAS precisa são verificados outra vez")
		}
	}
	for _, d := range slices.Sorted(maps.Keys(st.MirrorSeen)) {
		if _, ok := now[d]; !ok && owner[d] != "" && owner[d] != "npm" && !fileExists(filepath.Join(root, d, "docker-compose.yml")) { // a clustered one is left out, not gone
			a.alert(owner[d], "a pasta "+d+" já não está no espelho (ou perdeu o docker-compose.yml): o failover deste serviço não arranca")
		}
	}
	st.MirrorSeen = now
	st.MirrorNew = slices.DeleteFunc(st.MirrorNew, func(d string) bool { _, ok := now[d]; return !ok || owner[d] != "" })
	if changed {
		go a.scanImages()
	}
}

// snapshotTest is a failover's first step, tried: the snapshots folder is a
// btrfs volume, the mirror's, so a reflink copy of it works.
func (a *Agent) snapshotTest(c Config) error {
	test := filepath.Join(c.Paths.SnapshotsDir, "failover-teste-"+time.Now().Format("20060102-150405"))
	_ = os.Mkdir(c.Paths.SnapshotsDir, 0o755) // one level: under a parent that exists
	if err := a.sys.Run("btrfs", "subvolume", "create", test); err != nil {
		return fmt.Errorf("snapshot de teste do espelho falhou (a pasta dos snapshots tem de estar num subvolume btrfs, no mesmo volume do espelho): %w", err)
	}
	// one file is enough to prove the reflink (same btrfs volume); the mirror itself is not copied
	err := a.sys.Run("cp", "--reflink=always", filepath.Join(c.Paths.MirrorSubvol, c.Paths.MirrorRoot, c.NPM.Dir, "docker-compose.yml"), test+"/")
	if derr := a.sys.Run("btrfs", "subvolume", "delete", test); err == nil {
		err = derr
	}
	if err != nil {
		return fmt.Errorf("snapshot de teste do espelho falhou (a pasta dos snapshots tem de estar num subvolume btrfs, no mesmo volume do espelho): %w", err)
	}
	return nil
}

// Verdict is the answer to "if the server fell now, would the failover
// work?", checked after every scan of the images: every problem found.
type Verdict struct {
	At       time.Time `json:"at,omitzero"`
	Problems []string  `json:"problems,omitempty"`
}

// checkReady makes the verdict, outside mu: a token check against the
// Technitium and a test snapshot. A problem that was not there the last
// time is emailed; all gone is an event.
func (a *Agent) checkReady() {
	a.mu.Lock()
	c, stacks := a.cfg, maps.Clone(a.st.Images) // read without mu below: a service removed meanwhile edits the map
	stale, changed := a.st.MirrorStale, a.st.MirrorChanged
	a.mu.Unlock()

	var p []string
	b, _ := os.ReadFile(c.DNS.TokenFile)
	tok := strings.TrimSpace(string(b))
	if err := testToken(a.sys, c.DNS.APIURL, c.DNS.Zone, tok); err != nil {
		p = append(p, "o token do Technitium não serve: "+err.Error())
	} else {
		p = append(p, failoverProblems(a.sys, c, tok)...)
		p = append(p, ownRecords(a.sys, c, tok)...)
	}
	if err := a.snapshotTest(c); err != nil {
		p = append(p, err.Error())
	}
	if stale {
		p = append(p, "o espelho não muda desde "+changed.Format("02/01"))
	}
	names := []string{"npm"}
	for _, sv := range c.Services {
		names = append(names, sv.Name)
	}
	for _, n := range names {
		st, ok := stacks[n]
		if !ok {
			continue // not scanned yet
		}
		if st.Err != "" {
			p = append(p, n+": o compose não se lê: "+st.Err)
		}
		if m := missingImages(st); m > 0 {
			p = append(p, fmt.Sprintf("%s: %d %s em falta no TNAS (precisaria da internet)", n, m, map[bool]string{true: "imagem", false: "imagens"}[m == 1]))
		}
		for _, note := range st.Notes {
			p = append(p, n+": "+note)
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.now = time.Now()
	old := a.st.Verdict.Problems
	a.st.Verdict = Verdict{a.now, p}
	var fresh []string
	for _, x := range p {
		if !slices.Contains(old, x) {
			fresh = append(fresh, x)
		}
	}
	switch {
	case fresh != nil:
		a.alert("", "problemas para um failover: "+strings.Join(fresh, "; "))
	case p == nil && old != nil:
		a.event("", "pronto para failover outra vez")
	}
	a.save()
}

// ownRecords finds the services' names that have an A or AAAA of their own: the
// add (overwrite=true) would replace it and the return would delete it.
func ownRecords(sys System, c Config, tok string) []string {
	var p []string
	for _, sv := range c.Services {
		var rec struct {
			Response struct {
				Records []techRecord `json:"records"`
			} `json:"response"`
		}
		if technitiumJSON(sys, c.DNS.APIURL, "zones/records/get", url.Values{"domain": {sv.Host}, "zone": {c.DNS.Zone}}, tok, &rec) != nil {
			continue // unreadable is not a record: the token test above is the one that complains
		}
		for _, r := range rec.Response.Records {
			if (r.Type == "A" || r.Type == "AAAA") && r.RData.IPAddress != c.TNASIP { // the TNAS's is the record the agent itself writes
				p = append(p, sv.Name+": "+sv.Host+" tem um registo "+r.Type+" próprio ("+r.RData.IPAddress+"): o failover substituía-o e o regresso apagava-o; apaga-o ou deixa o nome seguir o *")
			}
		}
	}
	return p
}

func missingImages(st Stack) int {
	n := 0
	for _, im := range st.Images {
		if !im.Present {
			n++
		}
	}
	return n
}

// countImages counts the images the stacks need, each once however many stacks use it,
// how many the TNAS has and their size.
func countImages(stacks map[string]Stack) (present, total int, size int64) {
	seen := map[string]bool{}
	for _, st := range stacks {
		for _, im := range st.Images {
			if seen[im.Ref] {
				continue
			}
			seen[im.Ref] = true
			total++
			if im.Present {
				present++
				size += im.Size
			}
		}
	}
	return present, total, size
}
