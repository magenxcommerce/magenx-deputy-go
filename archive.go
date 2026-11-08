package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
)

// checkArchive reads a .tar.gz before it is extracted and refuses entries that could write or point
// outside the release, or carry privileges: absolute paths, "..", symlinks or hard links leaving the
// release, devices, fifos and setuid/setgid/sticky bits. stripComponents drops leading path elements
// the same way tar --strip-components does.
func checkArchive(file string, stripComponents int) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			// pax global header, e.g. the commit id in GitHub tarballs
			continue
		}
		name, ok := stripPath(hdr.Name, stripComponents)
		if !ok {
			continue
		}
		if err := checkArchiveEntry(hdr, name, stripComponents); err != nil {
			return fmt.Errorf("archive entry %q: %w", hdr.Name, err)
		}
	}
}

func checkArchiveEntry(hdr *tar.Header, name string, stripComponents int) error {
	if path.IsAbs(hdr.Name) {
		return errors.New("absolute path")
	}
	if escapes(hdr.Name) || escapes(name) {
		return errors.New("path leaves the release")
	}
	if hdr.Mode&(04000|02000|01000) != 0 {
		return fmt.Errorf("setuid, setgid or sticky bit (mode %s)", strconv.FormatInt(hdr.Mode, 8))
	}
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeRegA, tar.TypeDir, tar.TypeGNULongName, tar.TypeGNULongLink, tar.TypeXHeader:
		return nil
	case tar.TypeSymlink:
		if path.IsAbs(hdr.Linkname) {
			return fmt.Errorf("symlink to absolute path %q", hdr.Linkname)
		}
		if escapes(path.Join(path.Dir(name), hdr.Linkname)) {
			return fmt.Errorf("symlink %q leaves the release", hdr.Linkname)
		}
		return nil
	case tar.TypeLink:
		// a hard link names another entry of the archive, tar strips it like the entry name
		target, ok := stripPath(hdr.Linkname, stripComponents)
		if path.IsAbs(hdr.Linkname) || !ok || escapes(target) {
			return fmt.Errorf("hard link %q leaves the release", hdr.Linkname)
		}
		return nil
	default:
		return fmt.Errorf("unsupported entry type %q", string(hdr.Typeflag))
	}
}

// escapes reports whether a slash path relative to the release root points above it.
func escapes(p string) bool {
	clean := path.Clean(p)
	return clean == ".." || strings.HasPrefix(clean, "../")
}

// stripPath drops n leading elements; ok is false when nothing is left (tar skips such entries).
func stripPath(name string, n int) (string, bool) {
	name = strings.TrimPrefix(name, "./")
	for i := 0; i < n; i++ {
		_, rest, found := strings.Cut(name, "/")
		if !found {
			return "", false
		}
		name = rest
	}
	name = strings.TrimSuffix(name, "/")
	return name, name != ""
}

// extractArchive checks src, then extracts it into dst as the runner identity. Owners and permission
// bits from the archive are not kept: files belong to the identity, modes follow its umask.
func extractArchive(ctx context.Context, r Runner, src, dst string, stripComponents int) error {
	if err := checkArchive(src, stripComponents); err != nil {
		return err
	}
	args := []string{"-xzf", src, "-C", dst, "--no-same-owner", "--no-same-permissions"}
	if stripComponents > 0 {
		args = append(args, "--strip-components="+strconv.Itoa(stripComponents))
	}
	if err := r.Run(ctx, dst, nil, "tar", args...); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	return nil
}
