package logging

import (
	"context"
	"os"
	"path/filepath"

	"github.com/datawire/dlib/dlog"
	"github.com/sirupsen/logrus"
	"github.com/telepresenceio/telepresence/v2/pkg/filelocation"
)

// InitContext sets up standard Telepresence logging for a background process
func InitContext(ctx context.Context, name string) (context.Context, error) {
	logger := logrus.StandardLogger()
	logger.SetLevel(logrus.DebugLevel)

	if IsTerminal(int(os.Stdout.Fd())) {
		logger.Formatter = NewFormatter("15:04:05")
	} else {
		logger.Formatter = NewFormatter("2006/01/02 15:04:05")
		dir, err := filelocation.AppUserLogDir(ctx)
		if err != nil {
			return ctx, err
		}
		rf, err := OpenRotatingFile(filepath.Join(dir, name+".log"), "20060102T150405", true, true, 0600, NewRotateOnce(), 5)
		if err != nil {
			return ctx, err
		}
		logger.SetOutput(rf)
	}
	return dlog.WithLogger(ctx, dlog.WrapLogrus(logger)), nil
}
