//go:build darwin

package rdp

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// This opt-in integration test uses a private simulated client filesystem and
// the real macOS mount tool. It never mounts a user's redirected drive.
func TestDriveNativeWebDAVMount(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_WEBDAV_NATIVE") != "1" {
		t.Skip("set ANYKVM_TEST_WEBDAV_NATIVE=1 to test the native macOS WebDAV client")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed for the test's private terminal")
	}
	peer := newDrivePeer(t, 3)
	peer.c.drives.mu.Lock()
	endpoint := peer.c.drives.url
	peer.c.drives.mu.Unlock()
	mountpoint := t.TempDir()
	// -S suppresses authentication/disconnect UI; -i uses the private terminal.
	// Credentials below belong only to the in-memory test server.
	const probe = `
import os,pty,select,signal,subprocess,sys,time
url,mountpoint=sys.argv[1:]
pid,fd=pty.fork()
if pid==0:
 os.execv('/sbin/mount_webdav',['mount_webdav','-i','-S','-o','rdonly,nobrowse',url,mountpoint])
output=b''; user=False; password=False; status=None; waited=False
try:
 deadline=time.monotonic()+20
 while time.monotonic()<deadline:
  ready,_,_=select.select([fd],[],[],0.1)
  if ready:
   try:chunk=os.read(fd,4096)
   except OSError:break
   if not chunk:break
   output+=chunk
   if not user and b'Username:' in output:os.write(fd,b'user\n');user=True
   if not password and b'Password:' in output:os.write(fd,b'secret\n');password=True
  done,status=os.waitpid(pid,os.WNOHANG)
  if done:waited=True;break
 if not waited:
  done,status=os.waitpid(pid,os.WNOHANG)
  if not done:os.kill(pid,signal.SIGTERM);_,status=os.waitpid(pid,0)
  waited=True
 if not os.path.ismount(mountpoint):
  raise RuntimeError('native mount failed: status='+str(status)+' output='+repr(output))
 # Verify the mounted root exposes the simulated client's drive.
 if 'c' not in os.listdir(mountpoint):raise RuntimeError('drive missing from mounted root')
 print('Native WebDAV root authenticated, mounted read-only, and listed successfully')
finally:
 if not waited:
  try:os.kill(pid,signal.SIGTERM);os.waitpid(pid,0)
  except ProcessLookupError:pass
 if os.path.ismount(mountpoint):
  result=subprocess.run(['/sbin/umount',mountpoint],capture_output=True,timeout=5)
  if result.returncode:raise RuntimeError('test mount could not be unmounted')
 os.close(fd)
`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, python, "-c", probe, endpoint, mountpoint).CombinedOutput()
	if err != nil {
		t.Fatalf("native WebDAV mount: %v\n%s", err, out)
	}
	t.Log(string(out))
}
