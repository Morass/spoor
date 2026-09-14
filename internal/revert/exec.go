package revert

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/morass/spoor/internal/store"
)

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

func crontab(st *store.Store, a Action) error {
	var cmd *exec.Cmd
	if a.Hash == "" {
		cmd = exec.Command("crontab", "-r")
	} else {
		if _, err := os.Stat(st.ObjectPath(a.Hash)); err != nil {
			return err
		}
		cmd = exec.Command("crontab", st.ObjectPath(a.Hash))
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("crontab: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
