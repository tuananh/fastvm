# fastvm — VHS demo tape
#
# Shows the basic fastvm lifecycle: a real microVM boots from a base disk,
# accepts commands over vsock, pauses, pages its memory to disk, wakes again,
# and is deleted.
#
# Record (from the repo root, with a daemon already running):
#     vhs docs/demo.tape
#
# Or let hack/demo-vhs.sh start a throwaway daemon, build the demo image and
# record everything in one command:
#     hack/demo-vhs.sh
#
# Requirements: fastvmd reachable via $FASTVM_SOCKET (or /run/fastvm/fastvm.sock)
# and the base image demo:latest built from hack/Dockerfile.example.

Output docs/demo.gif

Set Shell "bash"
Set FontSize 20
Set Width 1200
Set Height 640
Set Padding 20
Set Margin 30
Set MarginFill "#0b0e14"
Set BorderRadius 10
Set WindowBar "Colorful"
Set WindowBarSize 36
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set Framerate 30
Set PlaybackSpeed 1.3
Set CursorBlink false

# --- what is fastvm -------------------------------------------------------
Type "bin/fastvm version"
Sleep 300ms
Enter
Sleep 700ms

Type "bin/fastvm image ls"
Sleep 300ms
Enter
Sleep 1200ms

# --- boot a microVM from the image ---------------------------------------
Type "bin/fastvm run --image demo:latest --name web --no-auto-sleep -- /bin/sleep 3600"
Sleep 300ms
Enter
Sleep 2200ms

Type "bin/fastvm ls"
Sleep 300ms
Enter
Sleep 1300ms

# --- exec into the guest over virtio-vsock -------------------------------
Type "bin/fastvm exec web -- uname -a"
Sleep 300ms
Enter
Sleep 1300ms

Type "bin/fastvm exec web -- /bin/sh -c 'cat /etc/motd'"
Sleep 300ms
Enter
Sleep 1300ms

Type "bin/fastvm exec web -- /bin/sh -c 'echo survived deep sleep > /tmp/greeting'"
Sleep 300ms
Enter
Sleep 1300ms

# --- pause / resume (guest RAM stays resident) ---------------------------
Type "bin/fastvm pause web"
Sleep 300ms
Enter
Sleep 600ms

Type "bin/fastvm ls"
Sleep 300ms
Enter
Sleep 1300ms

Type "bin/fastvm resume web"
Sleep 300ms
Enter
Sleep 800ms

# --- deep pause (guest RAM paged to disk) / wake -------------------------
Type "bin/fastvm pause web --deep"
Sleep 300ms
Enter
Sleep 900ms

Type "bin/fastvm ls"
Sleep 300ms
Enter
Sleep 1300ms

Type "bin/fastvm resume web"
Sleep 300ms
Enter
Sleep 900ms

Type "bin/fastvm exec web -- cat /tmp/greeting"
Sleep 300ms
Enter
Sleep 1500ms

# --- introspection and cleanup -------------------------------------------
Type "bin/fastvm events -n 8"
Sleep 300ms
Enter
Sleep 2500ms

Type "bin/fastvm rm web"
Sleep 300ms
Enter
Sleep 600ms

Type "bin/fastvm ls"
Sleep 300ms
Enter
Sleep 2000ms
