package storage

import "os"

const tarballPartialSuffix = ".partial"

// TarballPartialPath is the temporary path a download writes before Set publishes it
// onto the final cache path. Publishing happens under the same lock as expiration cleanup.
func TarballPartialPath(finalPath string) string {
	return finalPath + tarballPartialSuffix
}

// publishPartialTarball moves a downloaded temp file onto its final path.
// A missing temp file means the object is already at finalPath.
func publishPartialTarball(finalPath string) error {
	partial := TarballPartialPath(finalPath)
	if _, err := os.Stat(partial); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.Rename(partial, finalPath)
}
