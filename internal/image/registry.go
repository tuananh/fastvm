package image

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"fastvm/internal/types"
)

// Pull downloads ref from its OCI registry, flattens the image layers into a
// temporary rootfs and writes a base disk.
func (b *FSBuilder) Pull(ctx context.Context, spec PullSpec) (types.BaseDisk, error) {
	if err := ctx.Err(); err != nil {
		return types.BaseDisk{}, err
	}
	ref := spec.Ref
	if ref == "" {
		return types.BaseDisk{}, fmt.Errorf("image: reference is required")
	}
	parsed, err := b.parseReference(ref)
	if err != nil {
		return types.BaseDisk{}, err
	}
	img, err := b.remoteImage(ctx, parsed)
	if err != nil {
		return types.BaseDisk{}, fmt.Errorf("image: pull %q: %w", ref, err)
	}
	key, err := b.sourceKey(img, spec)
	if err != nil {
		return types.BaseDisk{}, err
	}
	if hex, ok := b.lookupSource(key); ok {
		if disk, ok := b.loadDisk(hex); ok {
			if err := b.recordTag(ref, hex); err != nil {
				return types.BaseDisk{}, err
			}
			disk.Name, disk.Tag = ref, ref
			return disk, nil
		}
		_ = os.Remove(b.sourcePath(key))
	}
	rootfs, cleanup, err := b.registryRootfsFromImage(ctx, ref, img)
	if err != nil {
		return types.BaseDisk{}, err
	}
	defer cleanup()
	disk, err := b.makeBaseDisk(ctx, baseDiskRequest{
		srcRootfs: rootfs,
		tag:       ref,
		sizeGB:    spec.SizeGB,
		agentPath: spec.AgentPath,
		initPath:  spec.InitPath,
		source:    "registry",
	})
	if err != nil {
		return types.BaseDisk{}, err
	}
	hex, ok := digestHexFromRef(disk.Digest)
	if !ok {
		return types.BaseDisk{}, fmt.Errorf("image: invalid disk digest %q", disk.Digest)
	}
	if err := b.recordSource(key, hex); err != nil {
		return types.BaseDisk{}, err
	}
	return disk, nil
}

// registryRootfs pulls ref and extracts its flattened rootfs. No Docker daemon
// or CLI is involved; registry credentials come from the Docker config
// keychain.
func (b *FSBuilder) registryRootfs(ctx context.Context, ref string) (string, func(), error) {
	parsed, err := b.parseReference(ref)
	if err != nil {
		return "", nil, err
	}
	img, err := b.remoteImage(ctx, parsed)
	if err != nil {
		return "", nil, fmt.Errorf("image: pull %q: %w", ref, err)
	}
	return b.registryRootfsFromImage(ctx, ref, img)
}

func (b *FSBuilder) parseReference(ref string) (name.Reference, error) {
	var opts []name.Option
	if b.insecureRegistry {
		opts = append(opts, name.Insecure)
	}
	parsed, err := name.ParseReference(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("image: parse reference %q: %w", ref, err)
	}
	return parsed, nil
}

func (b *FSBuilder) remoteImage(ctx context.Context, ref name.Reference) (v1.Image, error) {
	return remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
	)
}

// registryRootfsFromImage extracts an already-fetched image, applying layers
// from the shared compressed-blob cache. Layer extraction is sequential (later
// layers win); missing blobs are fetched concurrently.
func (b *FSBuilder) registryRootfsFromImage(ctx context.Context, ref string, img v1.Image) (string, func(), error) {
	layers, err := img.Layers()
	if err != nil {
		return "", nil, fmt.Errorf("image: layers for %q: %w", ref, err)
	}
	paths, err := b.ensureLayersCached(ctx, layers)
	if err != nil {
		return "", nil, err
	}
	workDir, err := os.MkdirTemp(b.workDir, ".fastvm-pull-*")
	if err != nil {
		return "", nil, fmt.Errorf("image: create pull workspace: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(workDir) }
	rootfs := workDir + "/rootfs"
	if err := os.MkdirAll(rootfs, 0o700); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("image: create rootfs dir: %w", err)
	}
	for i, layer := range layers {
		mt, err := layer.MediaType()
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("image: layer media type: %w", err)
		}
		rc, err := openLayerTar(paths[i], mt)
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("image: open cached layer: %w", err)
		}
		extractErr := extractTar(rootfs, rc)
		closeErr := rc.Close()
		if extractErr != nil {
			cleanup()
			return "", nil, fmt.Errorf("image: unpack layer %d of %q: %w", i, ref, extractErr)
		}
		if closeErr != nil {
			cleanup()
			return "", nil, fmt.Errorf("image: close layer %d of %q: %w", i, ref, closeErr)
		}
	}
	return rootfs, cleanup, nil
}
