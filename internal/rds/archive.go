package rds

import "time"

// ArchiveFile is one archived tarball in an archive.rds listing.
type ArchiveFile struct {
	// Path is "<pkg>/<pkg>_<version>.tar.gz", the row name R tools
	// append to src/contrib/Archive/.
	Path string
	Size int64
	// Time fills mtime, ctime and atime; packyard uses the publish
	// time.
	Time time.Time
}

// ArchivePackage is one package's archived versions, oldest first:
// remotes::install_version() scans the rows from the end.
type ArchivePackage struct {
	Name  string
	Files []ArchiveFile
}

// fileMode is the mode reported for every archived tarball (0644).
const fileMode = 0o644

// fileOwner is the uname/grname reported for every archived tarball.
const fileOwner = "packyard"

// Archive builds the object CRAN serves as src/contrib/Meta/archive.rds:
// a named list with one data frame per package, each shaped like
// file.info() output with the tarball paths as row names. pkgs must
// already be sorted the way the listing should appear.
func Archive(pkgs []ArchivePackage) Object {
	names := make([]string, len(pkgs))
	frames := make([]Object, len(pkgs))
	for i, p := range pkgs {
		names[i] = p.Name
		frames[i] = fileInfoFrame(p.Files)
	}
	return List(frames...).WithAttr("names", Strings(names...))
}

func fileInfoFrame(files []ArchiveFile) Object {
	n := len(files)
	var (
		paths = make([]string, n)
		sizes = make([]float64, n)
		isdir = make([]bool, n)
		modes = make([]int32, n)
		times = make([]float64, n)
		ids   = make([]int32, n)
		users = make([]string, n)
	)
	for i, f := range files {
		paths[i] = f.Path
		sizes[i] = float64(f.Size)
		modes[i] = fileMode
		times[i] = float64(f.Time.UnixNano()) / 1e9
		users[i] = fileOwner
	}
	posix := func() Object {
		return Doubles(times...).WithAttr("class", Strings("POSIXct", "POSIXt"))
	}
	cols := []string{"size", "isdir", "mode", "mtime", "ctime", "atime", "uid", "gid", "uname", "grname"}
	return List(
		Doubles(sizes...),
		Logicals(isdir...),
		Ints(modes...).WithAttr("class", Strings("octmode")),
		posix(), posix(), posix(),
		Ints(ids...),
		Ints(ids...),
		Strings(users...),
		Strings(users...),
	).
		WithAttr("names", Strings(cols...)).
		WithAttr("class", Strings("data.frame")).
		WithAttr("row.names", Strings(paths...))
}
