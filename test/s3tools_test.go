package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/trukeio/f1/pkg/f1client"
)

// External S3 tools (AWS CLI, rclone, MinIO client) run their scripts in
// test/s3tools/ against an in-process server, on a flat and a tree layout
// bucket. A tool that is not installed is skipped with a message, unless
// F1_REQUIRE_S3TOOLS is set (CI installs them with test/s3tools/install.sh).
func TestS3Tools(t *testing.T) {
	for _, tool := range []struct{ name, bin string }{{"aws", "aws"}, {"rclone", "rclone"}, {"mc", "mc"}} {
		t.Run(tool.name, func(t *testing.T) {
			if _, err := exec.LookPath(tool.bin); err != nil {
				if os.Getenv("F1_REQUIRE_S3TOOLS") != "" {
					t.Fatalf("%s is not installed", tool.bin)
				}
				t.Skipf("%s is not installed; skipping test/s3tools/%s.sh", tool.bin, tool.name)
			}
			e := start(t, envOpts{})
			e.createBucket(t, "tools-flat")
			if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "tools-tree", Config: &f1client.NamespaceConfig{
				Layout: "tree", Retention: "none", Autocommit: true, Locks: "off", DefaultRef: "main"}}); err != nil {
				t.Fatal(err)
			}
			script, err := filepath.Abs(filepath.Join("s3tools", tool.name+".sh"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", script)
			k := e.s3keys["admin"]
			cmd.Env = append(os.Environ(), "S3_ENDPOINT="+e.s3url, "AWS_ACCESS_KEY_ID="+k.AccessKey, "AWS_SECRET_ACCESS_KEY="+k.Secret,
				"BUCKET=tools-flat", "TREE_BUCKET=tools-tree", "WORK="+t.TempDir(), "HOME="+t.TempDir())
			out, err := cmd.CombinedOutput()
			t.Logf("%s", out)
			if err != nil {
				t.Fatalf("%s.sh: %v", tool.name, err)
			}
		})
	}
}
