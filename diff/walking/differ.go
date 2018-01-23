package walking

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"time"

	"github.com/containerd/containerd/archive"
	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/metadata"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/plugin"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.DiffPlugin,
		ID:   "walking",
		Requires: []plugin.Type{
			plugin.MetadataPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			md, err := ic.Get(plugin.MetadataPlugin)
			if err != nil {
				return nil, err
			}

			ic.Meta.Platforms = append(ic.Meta.Platforms, platforms.DefaultSpec())
			return NewWalkingDiff(md.(*metadata.DB).ContentStore())
		},
	})
}

type walkingDiff struct {
	store content.Store
}

var emptyDesc = ocispec.Descriptor{}

// NewWalkingDiff is a generic implementation of diff.Differ.
// NewWalkingDiff is expected to work with any filesystem.
func NewWalkingDiff(store content.Store) (diff.Differ, error) {
	return &walkingDiff{
		store: store,
	}, nil
}

// Apply applies the content associated with the provided digests onto the
// provided mounts. Archive content will be extracted and decompressed if
// necessary.
func (s *walkingDiff) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount) (d ocispec.Descriptor, err error) {
	t1 := time.Now()
	defer func() {
		if err == nil {
			log.G(ctx).WithFields(logrus.Fields{
				"d":     time.Now().Sub(t1),
				"dgst":  desc.Digest,
				"size":  desc.Size,
				"media": desc.MediaType,
			}).Debugf("diff applied")
		}
	}()

	isCompressed, err := images.IsCompressedDiff(ctx, desc.MediaType)
	if err != nil {
		return emptyDesc, errors.Wrapf(errdefs.ErrNotImplemented, "unsupported diff media type: %v", desc.MediaType)
	}

	var ocidesc ocispec.Descriptor
	if err := mount.WithTempMount(ctx, mounts, func(root string) error {
		ra, err := s.store.ReaderAt(ctx, desc.Digest)
		if err != nil {
			return errors.Wrap(err, "failed to get reader from content store")
		}
		defer ra.Close()

		r := content.NewReader(ra)
		if isCompressed {
			ds, err := compression.DecompressStream(r)
			if err != nil {
				return err
			}
			defer ds.Close()
			r = ds
		}

		digester := digest.Canonical.Digester()
		rc := &readCounter{
			r: io.TeeReader(r, digester.Hash()),
		}

		if _, err := archive.Apply(ctx, root, rc); err != nil {
			return err
		}

		// Read any trailing data
		if _, err := io.Copy(ioutil.Discard, rc); err != nil {
			return err
		}

		ocidesc = ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageLayer,
			Size:      rc.c,
			Digest:    digester.Digest(),
		}
		return nil

	}); err != nil {
		return emptyDesc, err
	}
	return ocidesc, nil
}

// DiffMounts creates a diff between the given mounts and uploads the result
// to the content store.
func (s *walkingDiff) DiffMounts(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (d ocispec.Descriptor, err error) {
	var config diff.Config
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return emptyDesc, err
		}
	}

	if config.MediaType == "" {
		config.MediaType = ocispec.MediaTypeImageLayerGzip
	}

	var isCompressed bool
	switch config.MediaType {
	case ocispec.MediaTypeImageLayer:
	case ocispec.MediaTypeImageLayerGzip:
		isCompressed = true
	default:
		return emptyDesc, errors.Wrapf(errdefs.ErrNotImplemented, "unsupported diff media type: %v", config.MediaType)
	}

	var ocidesc ocispec.Descriptor
	if err := mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
		return mount.WithTempMount(ctx, upper, func(upperRoot string) error {
			var newReference bool
			if config.Reference == "" {
				newReference = true
				config.Reference = uniqueRef()
			}

			cw, err := s.store.Writer(ctx, config.Reference, 0, "")
			if err != nil {
				return errors.Wrap(err, "failed to open writer")
			}
			defer func() {
				if err != nil {
					cw.Close()
					if newReference {
						if err := s.store.Abort(ctx, config.Reference); err != nil {
							log.G(ctx).WithField("ref", config.Reference).Warnf("failed to delete diff upload")
						}
					}
				}
			}()
			if !newReference {
				if err := cw.Truncate(0); err != nil {
					return err
				}
			}

			if isCompressed {
				dgstr := digest.SHA256.Digester()
				compressed, err := compression.CompressStream(cw, compression.Gzip)
				if err != nil {
					return errors.Wrap(err, "failed to get compressed stream")
				}
				err = archive.WriteDiff(ctx, io.MultiWriter(compressed, dgstr.Hash()), lowerRoot, upperRoot)
				compressed.Close()
				if err != nil {
					return errors.Wrap(err, "failed to write compressed diff")
				}

				if config.Labels == nil {
					config.Labels = map[string]string{}
				}
				config.Labels["containerd.io/uncompressed"] = dgstr.Digest().String()
			} else {
				if err = archive.WriteDiff(ctx, cw, lowerRoot, upperRoot); err != nil {
					return errors.Wrap(err, "failed to write diff")
				}
			}

			var commitopts []content.Opt
			if config.Labels != nil {
				commitopts = append(commitopts, content.WithLabels(config.Labels))
			}

			dgst := cw.Digest()
			if err := cw.Commit(ctx, 0, dgst, commitopts...); err != nil {
				return errors.Wrap(err, "failed to commit")
			}

			info, err := s.store.Info(ctx, dgst)
			if err != nil {
				return errors.Wrap(err, "failed to get info from content store")
			}

			ocidesc = ocispec.Descriptor{
				MediaType: config.MediaType,
				Size:      info.Size,
				Digest:    info.Digest,
			}
			return nil
		})
	}); err != nil {
		return emptyDesc, err
	}

	return ocidesc, nil
}

type readCounter struct {
	r io.Reader
	c int64
}

func (rc *readCounter) Read(p []byte) (n int, err error) {
	n, err = rc.r.Read(p)
	rc.c += int64(n)
	return
}

func uniqueRef() string {
	t := time.Now()
	var b [3]byte
	// Ignore read failures, just decreases uniqueness
	rand.Read(b[:])
	return fmt.Sprintf("%d-%s", t.UnixNano(), base64.URLEncoding.EncodeToString(b[:]))
}
