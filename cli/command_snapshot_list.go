package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/object"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
)

var (
	snapshotListCommand              = snapshotCommands.Command("list", "List snapshots of files and directories.").Alias("ls")
	snapshotListPath                 = snapshotListCommand.Arg("source", "File or directory to show history of.").String()
	snapshotListIncludeIncomplete    = snapshotListCommand.Flag("incomplete", "Include incomplete.").Short('i').Bool()
	snapshotListShowHumanReadable    = snapshotListCommand.Flag("human-readable", "Show human-readable units").Default("true").Bool()
	snapshotListShowDelta            = snapshotListCommand.Flag("delta", "Include deltas.").Short('d').Bool()
	snapshotListShowItemID           = snapshotListCommand.Flag("manifest-id", "Include manifest item ID.").Short('m').Bool()
	snapshotListShowHashCache        = snapshotListCommand.Flag("hashcache", "Include hashcache object ID.").Bool()
	snapshotListShowRetentionReasons = snapshotListCommand.Flag("retention", "Include retention reasons.").Default("true").Bool()
	snapshotListShowModTime          = snapshotListCommand.Flag("mtime", "Include file mod time").Bool()
	shapshotListShowOwner            = snapshotListCommand.Flag("owner", "Include owner").Bool()
	maxResultsPerPath                = snapshotListCommand.Flag("max-results", "Maximum number of results.").Default("1000").Int()
)

func findSnapshotsForSource(mgr *snapshot.Manager, sourceInfo snapshot.SourceInfo) (manifestIDs []string, relPath string, err error) {
	for len(sourceInfo.Path) > 0 {
		list := mgr.ListSnapshotManifests(&sourceInfo)

		if len(list) > 0 {
			return list, relPath, nil
		}

		if len(relPath) > 0 {
			relPath = filepath.Base(sourceInfo.Path) + "/" + relPath
		} else {
			relPath = filepath.Base(sourceInfo.Path)
		}

		log.Printf("No snapshots of %v@%v:%v", sourceInfo.UserName, sourceInfo.Host, sourceInfo.Path)

		parentPath := filepath.Dir(sourceInfo.Path)
		if parentPath == sourceInfo.Path {
			break
		}
		sourceInfo.Path = parentPath
	}

	return nil, "", nil
}

func findManifestIDs(mgr *snapshot.Manager, source string) ([]string, string, error) {
	if source == "" {
		return mgr.ListSnapshotManifests(nil), "", nil
	}

	si, err := snapshot.ParseSourceInfo(source, getHostName(), getUserName())
	if err != nil {
		return nil, "", fmt.Errorf("invalid directory: '%s': %s", source, err)
	}

	manifestIDs, relPath, err := findSnapshotsForSource(mgr, si)
	if relPath != "" {
		relPath = "/" + relPath
	}

	return manifestIDs, relPath, err
}

func runSnapshotsCommand(ctx context.Context, rep *repo.Repository) error {
	mgr := snapshot.NewManager(rep)

	manifestIDs, relPath, err := findManifestIDs(mgr, *snapshotListPath)
	if err != nil {
		return err
	}

	manifests, err := mgr.LoadSnapshots(manifestIDs)
	if err != nil {
		return err
	}

	polMgr := snapshot.NewPolicyManager(rep)

	return outputManifestGroups(ctx, manifests, strings.Split(relPath, "/"), mgr, polMgr)
}

func outputManifestGroups(ctx context.Context, manifests []*snapshot.Manifest, relPathParts []string, mgr *snapshot.Manager, polMgr *snapshot.PolicyManager) error {
	separator := ""
	for _, snapshotGroup := range snapshot.GroupBySource(manifests) {
		src := snapshotGroup[0].Source
		fmt.Printf("%v%v\n", separator, src)
		separator = "\n"

		pol, _, err := polMgr.GetEffectivePolicy(src)
		if err != nil {
			log.Warn().Msgf("unable to determine effective policy for %v", src)
		} else {
			pol.RetentionPolicy.ComputeRetentionReasons(snapshotGroup)
		}
		if err := outputManifestFromSingleSource(ctx, snapshotGroup, relPathParts, mgr); err != nil {
			return err
		}
	}

	return nil
}

//nolint:gocyclo
func outputManifestFromSingleSource(ctx context.Context, manifests []*snapshot.Manifest, parts []string, mgr *snapshot.Manager) error {
	var count int
	var lastTotalFileSize int64

	manifests = snapshot.SortByTime(manifests, false)
	if len(manifests) > *maxResultsPerPath {
		manifests = manifests[len(manifests)-*maxResultsPerPath:]
	}

	for _, m := range manifests {
		root, err := mgr.SnapshotRoot(m)
		if err != nil {
			fmt.Printf("  %v <ERROR> %v\n", m.StartTime.Format("2006-01-02 15:04:05 MST"), err)
			continue
		}
		ent, err := getNestedEntry(ctx, root, parts)
		if err != nil {
			fmt.Printf("  %v <ERROR> %v\n", m.StartTime.Format("2006-01-02 15:04:05 MST"), err)
			continue
		}

		if _, ok := ent.(object.HasObjectID); !ok {
			log.Warn().Msgf("entry does not have object ID: %v", ent, err)
			continue
		}

		var bits []string
		if m.IncompleteReason != "" {
			if !*snapshotListIncludeIncomplete {
				continue
			}
			bits = append(bits, "incomplete:"+m.IncompleteReason)
		}

		if *snapshotListShowHumanReadable {
			bits = append(bits, units.BytesStringBase10(ent.Metadata().FileSize))
		} else {
			bits = append(bits, fmt.Sprintf("%v", ent.Metadata().FileSize))
		}
		bits = append(bits, fmt.Sprintf("%v", ent.Metadata().FileMode()))
		if *shapshotListShowOwner {
			bits = append(bits, fmt.Sprintf("uid:%v", ent.Metadata().UserID))
			bits = append(bits, fmt.Sprintf("gid:%v", ent.Metadata().GroupID))
		}
		if *snapshotListShowModTime {
			bits = append(bits, fmt.Sprintf("modified:%v", ent.Metadata().ModTime.Format(timeFormat)))
		}

		if *snapshotListShowItemID {
			bits = append(bits, "manifest:"+m.ID)
		}
		if *snapshotListShowHashCache {
			bits = append(bits, "hashcache:"+m.HashCacheID.String())
		}

		if *snapshotListShowDelta {
			bits = append(bits, deltaBytes(ent.Metadata().FileSize-lastTotalFileSize))
		}

		if d, ok := ent.(fs.Directory); ok {
			s := d.Summary()
			if s != nil {
				bits = append(bits, fmt.Sprintf("files:%v", s.TotalFileCount))
				bits = append(bits, fmt.Sprintf("dirs:%v", s.TotalDirCount))
			}
		}

		if *snapshotListShowRetentionReasons {
			if len(m.RetentionReasons) > 0 {
				bits = append(bits, "retention:"+strings.Join(m.RetentionReasons, ","))
			}
		}

		fmt.Printf(
			"  %v %v %v\n",
			m.StartTime.Format("2006-01-02 15:04:05 MST"),
			ent.(object.HasObjectID).ObjectID(),
			strings.Join(bits, " "),
		)

		count++
		if m.IncompleteReason == "" {
			lastTotalFileSize = m.Stats.TotalFileSize
		}
	}

	return nil
}

func deltaBytes(b int64) string {
	if b > 0 {
		return "(+" + units.BytesStringBase10(b) + ")"
	}

	return ""
}

func init() {
	snapshotListCommand.Action(repositoryAction(runSnapshotsCommand))
}
