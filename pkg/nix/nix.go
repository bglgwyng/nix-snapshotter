/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package nix

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/overlay/overlayutils"
	"github.com/containerd/containerd/snapshots/storage"
	"github.com/containerd/continuity/fs"
	"github.com/pdtpartners/nix-snapshotter/pkg/nix2container"
	"github.com/sirupsen/logrus"
)

const (
	// upperdirKey is a key of an optional label to each snapshot.
	// This optional label of a snapshot contains the location of "upperdir" where
	// the change set between this snapshot and its parent is stored.
	upperdirKey = "containerd.io/snapshot/overlay.upperdir"

	labelSnapshotRef = "containerd.io/snapshot.ref"

	defaultNixTool = "nix"
)

// SnapshotterConfig is used to configure the overlay snapshotter instance
type SnapshotterConfig struct {
	asyncRemove   bool
	upperdirLabel bool
	fuse          bool
}

// Opt is an option to configure the overlay snapshotter
type Opt func(config *SnapshotterConfig) error

// AsynchronousRemove defers removal of filesystem content until
// the Cleanup method is called. Removals will make the snapshot
// referred to by the key unavailable and make the key immediately
// available for re-use.
func AsynchronousRemove(config *SnapshotterConfig) error {
	config.asyncRemove = true
	return nil
}

// WithUpperdirLabel adds as an optional label
// "containerd.io/snapshot/overlay.upperdir". This stores the location
// of the upperdir that contains the changeset between the labelled
// snapshot and its parent.
func WithUpperdirLabel(config *SnapshotterConfig) error {
	config.upperdirLabel = true
	return nil
}

// WithFuseOverlayfs changes the overlay mount type used to fuse-overlayfs, an
// FUSE implementation for overlayfs.
//
// See: https://github.com/containers/fuse-overlayfs
func WithFuseOverlayfs(config *SnapshotterConfig) error {
	config.fuse = true
	return nil
}

type snapshotter struct {
	root          string
	nixStoreDir   string
	ms            *storage.MetaStore
	asyncRemove   bool
	upperdirLabel bool
	indexOff      bool
	userxattr     bool // whether to enable "userxattr" mount option
	fuse          bool
}

// NewSnapshotter returns a Snapshotter which uses overlayfs. The overlayfs
// diffs are stored under the provided root. A metadata file is stored under
// the root.
func NewSnapshotter(root, nixStoreDir string, opts ...Opt) (snapshots.Snapshotter, error) {
	var config SnapshotterConfig
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	supportsDType, err := fs.SupportsDType(root)
	if err != nil {
		return nil, err
	}
	if !supportsDType {
		return nil, fmt.Errorf("%s does not support d_type. If the backing filesystem is xfs, please reformat with ftype=1 to enable d_type support", root)
	}
	ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, err
	}

	if err := os.Mkdir(filepath.Join(root, "snapshots"), 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	// figure out whether "userxattr" option is recognized by the kernel && needed
	userxattr, err := overlayutils.NeedsUserXAttr(root)
	if err != nil {
		logrus.WithError(err).Warnf("cannot detect whether \"userxattr\" option needs to be used, assuming to be %v", userxattr)
	}

	return &snapshotter{
		root:          root,
		nixStoreDir:   nixStoreDir,
		ms:            ms,
		asyncRemove:   config.asyncRemove,
		upperdirLabel: config.upperdirLabel,
		indexOff:      supportsIndex(),
		userxattr:     userxattr,
		fuse:          config.fuse,
	}, nil
}

// Stat returns the info for an active or committed snapshot by name or
// key.
//
// Should be used for parent resolution, existence checks and to discern
// the kind of snapshot.
func (o *snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return snapshots.Info{}, err
	}
	defer t.Rollback()
	id, info, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return snapshots.Info{}, err
	}

	if o.upperdirLabel {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		info.Labels[upperdirKey] = o.upperPath(id)
	}

	return info, nil
}

func (o *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return snapshots.Info{}, err
	}

	rollback := true
	defer func() {
		if rollback {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
	if err != nil {
		return snapshots.Info{}, err
	}

	if o.upperdirLabel {
		id, _, _, err := storage.GetInfo(ctx, info.Name)
		if err != nil {
			return snapshots.Info{}, err
		}
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		info.Labels[upperdirKey] = o.upperPath(id)
	}

	rollback = false
	if err := t.Commit(); err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

// Usage returns the resources taken by the snapshot identified by key.
//
// For active snapshots, this will scan the usage of the overlay "diff" (aka
// "upper") directory and may take some time.
//
// For committed snapshots, the value is returned from the metadata database.
func (o *snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return snapshots.Usage{}, err
	}
	id, info, usage, err := storage.GetInfo(ctx, key)
	t.Rollback() // transaction no longer needed at this point.

	if err != nil {
		return snapshots.Usage{}, err
	}

	if info.Kind == snapshots.KindActive {
		upperPath := o.upperPath(id)
		du, err := fs.DiskUsage(ctx, upperPath)
		if err != nil {
			// TODO(stevvooe): Consider not reporting an error in this case.
			return snapshots.Usage{}, err
		}

		usage = snapshots.Usage(du)
	}

	return usage, nil
}

func (o *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	var base snapshots.Info
	for _, opt := range opts {
		if err := opt(&base); err != nil {
			return nil, err
		}
	}

	mounts, err := o.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
	if err != nil {
		return nil, err
	}

	// Annotations with prefix `containerd.io/snapshot/` will be passed down by
	// the unpacker during CRI pull time. If this is a nix layer, then we need to
	// prepare gc roots to ensure nix doesn't GC the underlying paths while this
	// snapshot is alive.
	//
	// We also don't return any nix bind mounts because the unpacker just needs
	// to retrieve and unpack the layer tarball containing the nix store
	// mountpoints and copyToRoot symlinks. Returning nix bind mounts will error
	// due to the paths being read only.
	if _, ok := base.Labels[nix2container.NixLayerAnnotation]; ok {
		err = o.prepareNixGCRoots(ctx, key, base.Labels)
		return mounts, err
	}

	return o.withNixBindMounts(ctx, key, mounts)
}

func (o *snapshotter) prepareNixGCRoots(ctx context.Context, key string, labels map[string]string) error {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return err
	}
	defer t.Rollback()
	id, _, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return err
	}

	// Allow users to specify which nix to use. Perhaps this should be coming from
	// a label.
	nixTool := os.Getenv("NIX_TOOL")
	if nixTool == "" {
		nixTool = defaultNixTool
	}

	gcRootsDir := filepath.Join(o.root, "gcroots", id)
	log.G(ctx).Infof("Preparing nix gc roots at %s", gcRootsDir)
	for label, nixHash := range labels {
		if !strings.HasPrefix(label, nix2container.NixStorePrefixAnnotation) {
			continue
		}

		// nix build with a store path fetches a store path from the configured
		// substituters, if it doesn't already exist.
		nixPath := filepath.Join(o.nixStoreDir, nixHash)
		_, err = exec.Command(
			nixTool,
			"build",
			"--out-link",
			filepath.Join(gcRootsDir, nixHash),
			nixPath,
		).Output()
		if err != nil {
			return err
		}
	}

	return nil
}

func (o *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	mounts, err := o.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
	if err != nil {
		return nil, err
	}
	return o.withNixBindMounts(ctx, key, mounts)
}

// Mounts returns the mounts for the transaction identified by key. Can be
// called on an read-write or readonly transaction.
//
// This can be used to recover mounts after calling View or Prepare.
func (o *snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return nil, err
	}
	s, err := storage.GetSnapshot(ctx, key)
	t.Rollback()
	if err != nil {
		return nil, fmt.Errorf("failed to get active mount: %w", err)
	}

	return o.withNixBindMounts(ctx, key, o.mounts(s))
}

func (o *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	// grab the existing id
	id, _, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return err
	}

	usage, err := fs.DiskUsage(ctx, o.upperPath(id))
	if err != nil {
		return err
	}

	if _, err = storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...); err != nil {
		return fmt.Errorf("failed to commit snapshot: %w", err)
	}
	return t.Commit()
}

// Remove abandons the snapshot identified by key. The snapshot will
// immediately become unavailable and unrecoverable. Disk space will
// be freed up on the next call to `Cleanup`.
func (o *snapshotter) Remove(ctx context.Context, key string) (err error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	_, _, err = storage.Remove(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to remove: %w", err)
	}

	if !o.asyncRemove {
		var removals []string
		removals, err = o.getCleanupDirectories(ctx)
		if err != nil {
			return fmt.Errorf("unable to get directories for removal: %w", err)
		}

		// Remove directories after the transaction is closed, failures must not
		// return error since the transaction is committed with the removal
		// key no longer available.
		defer func() {
			if err == nil {
				for _, dir := range removals {
					if err := os.RemoveAll(dir); err != nil {
						log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
					}
				}
			}
		}()

	}

	return t.Commit()
}

// Walk the snapshots.
func (o *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, fs ...string) error {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return err
	}
	defer t.Rollback()
	if o.upperdirLabel {
		return storage.WalkInfo(ctx, func(ctx context.Context, info snapshots.Info) error {
			id, _, _, err := storage.GetInfo(ctx, info.Name)
			if err != nil {
				return err
			}
			if info.Labels == nil {
				info.Labels = make(map[string]string)
			}
			info.Labels[upperdirKey] = o.upperPath(id)
			return fn(ctx, info)
		}, fs...)
	}
	return storage.WalkInfo(ctx, fn, fs...)
}

// Cleanup cleans up disk resources from removed or abandoned snapshots
func (o *snapshotter) Cleanup(ctx context.Context) error {
	cleanup, err := o.cleanupDirectories(ctx)
	if err != nil {
		return err
	}

	for _, dir := range cleanup {
		if err := os.RemoveAll(dir); err != nil {
			log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
		}
	}

	return nil
}

func (o *snapshotter) cleanupDirectories(ctx context.Context) ([]string, error) {
	// Get a write transaction to ensure no other write transaction can be entered
	// while the cleanup is scanning.
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return nil, err
	}

	defer t.Rollback()
	return o.getCleanupDirectories(ctx)
}

func (o *snapshotter) getCleanupDirectories(ctx context.Context) ([]string, error) {
	ids, err := storage.IDMap(ctx)
	if err != nil {
		return nil, err
	}

	snapshotDir := filepath.Join(o.root, "snapshots")
	fd, err := os.Open(snapshotDir)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	dirs, err := fd.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	cleanup := []string{}
	gcRootsDir := filepath.Join(o.root, "gcroots")
	for _, d := range dirs {
		if _, ok := ids[d]; ok {
			continue
		}
		// Cleanup the snapshot and its corresponding nix gc roots.
		cleanup = append(cleanup, filepath.Join(snapshotDir, d))
		cleanup = append(cleanup, filepath.Join(gcRootsDir, d))
	}

	return cleanup, nil
}

func (o *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ []mount.Mount, err error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return nil, err
	}

	var td, path string
	defer func() {
		if err != nil {
			if td != "" {
				if err1 := os.RemoveAll(td); err1 != nil {
					log.G(ctx).WithError(err1).Warn("failed to cleanup temp snapshot directory")
				}
			}
			if path != "" {
				if err1 := os.RemoveAll(path); err1 != nil {
					log.G(ctx).WithError(err1).WithField("path", path).Error("failed to reclaim snapshot directory, directory may need removal")
					err = fmt.Errorf("failed to remove path: %v: %w", err1, err)
				}
			}
		}
	}()

	snapshotDir := filepath.Join(o.root, "snapshots")
	td, err = o.prepareDirectory(ctx, snapshotDir, kind)
	if err != nil {
		if rerr := t.Rollback(); rerr != nil {
			log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
		}
		return nil, fmt.Errorf("failed to create prepare snapshot dir: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	s, err := storage.CreateSnapshot(ctx, kind, key, parent, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot: %w", err)
	}

	if len(s.ParentIDs) > 0 {
		st, err := os.Stat(o.upperPath(s.ParentIDs[0]))
		if err != nil {
			return nil, fmt.Errorf("failed to stat parent: %w", err)
		}

		stat := st.Sys().(*syscall.Stat_t)

		if err := os.Lchown(filepath.Join(td, "fs"), int(stat.Uid), int(stat.Gid)); err != nil {
			return nil, fmt.Errorf("failed to chown: %w", err)
		}
	}

	path = filepath.Join(snapshotDir, s.ID)
	if err = os.Rename(td, path); err != nil {
		return nil, fmt.Errorf("failed to rename: %w", err)
	}
	td = ""

	rollback = false
	if err = t.Commit(); err != nil {
		return nil, fmt.Errorf("commit failed: %w", err)
	}

	return o.mounts(s), nil
}

func (o *snapshotter) prepareDirectory(ctx context.Context, snapshotDir string, kind snapshots.Kind) (string, error) {
	td, err := os.MkdirTemp(snapshotDir, "new-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	if err := os.Mkdir(filepath.Join(td, "fs"), 0755); err != nil {
		return td, err
	}

	if kind == snapshots.KindActive {
		if err := os.Mkdir(filepath.Join(td, "work"), 0711); err != nil {
			return td, err
		}
	}

	return td, nil
}

func (o *snapshotter) mounts(s storage.Snapshot) []mount.Mount {
	if len(s.ParentIDs) == 0 {
		// if we only have one layer/no parents then just return a bind mount as overlay
		// will not work
		roFlag := "rw"
		if s.Kind == snapshots.KindView {
			roFlag = "ro"
		}

		return []mount.Mount{
			{
				Source: o.upperPath(s.ID),
				Type:   "bind",
				Options: []string{
					roFlag,
					"rbind",
				},
			},
		}
	}
	var options []string

	// set index=off when mount overlayfs
	if o.indexOff {
		options = append(options, "index=off")
	}

	if o.userxattr {
		options = append(options, "userxattr")
	}

	if s.Kind == snapshots.KindActive {
		options = append(options,
			fmt.Sprintf("workdir=%s", o.workPath(s.ID)),
			fmt.Sprintf("upperdir=%s", o.upperPath(s.ID)),
		)
	} else if len(s.ParentIDs) == 1 {
		return []mount.Mount{
			{
				Source: o.upperPath(s.ParentIDs[0]),
				Type:   "bind",
				Options: []string{
					"ro",
					"rbind",
				},
			},
		}
	}

	parentPaths := make([]string, len(s.ParentIDs))
	for i := range s.ParentIDs {
		parentPaths[i] = o.upperPath(s.ParentIDs[i])
	}

	options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(parentPaths, ":")))
	return []mount.Mount{
		{
			Type:    o.overlayMountType(),
			Source:  "overlay",
			Options: options,
		},
	}
}

func (o *snapshotter) upperPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "fs")
}

func (o *snapshotter) workPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "work")
}

func (o *snapshotter) overlayMountType() string {
	if o.fuse {
		return "fuse3.fuse-overlayfs"
	}
	return "overlay"
}

// Close closes the snapshotter
func (o *snapshotter) Close() error {
	return o.ms.Close()
}

func (o *snapshotter) withNixBindMounts(ctx context.Context, key string, mounts []mount.Mount) ([]mount.Mount, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return nil, err
	}
	defer t.Rollback()

	// Add a read only bind mount for every nix path required for the current
	// snapshot and all its parents.
	pathsSeen := make(map[string]struct{})
	for currentKey := key; currentKey != ""; {
		_, info, _, err := storage.GetInfo(ctx, currentKey)
		if err != nil {
			return nil, err
		}

		for label, nixHash := range info.Labels {
			if !strings.HasPrefix(label, nix2container.NixStorePrefixAnnotation) {
				continue
			}

			// Avoid duplicate mounts.
			_, ok := pathsSeen[nixHash]
			if ok {
				continue
			}
			pathsSeen[nixHash] = struct{}{}

			storePath := filepath.Join(o.nixStoreDir, nixHash)
			mounts = append(mounts, mount.Mount{
				Source: storePath,
				Type:   "bind",
				Target: storePath,
				Options: []string{
					"ro",
					"rbind",
				},
			})
		}

		currentKey = info.Parent
	}
	return mounts, nil
}

// supportsIndex checks whether the "index=off" option is supported by the kernel.
func supportsIndex() bool {
	if _, err := os.Stat("/sys/module/overlay/parameters/index"); err == nil {
		return true
	}
	return false
}