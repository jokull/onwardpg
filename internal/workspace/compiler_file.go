package workspace

import (
	"fmt"
	"os"
)

func checkRegularCompilerPath(name string) error {
	info, err := os.Stat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("DDL export input must be a regular file")
	}
	return nil
}

func checkRegularCompilerFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("DDL export input must be a regular file")
	}
	return nil
}
