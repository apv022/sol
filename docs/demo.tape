# Record the README demo GIF with https://github.com/charmbracelet/vhs :
#   vhs docs/demo.tape
# Prepare first: a node running elsewhere with a "navidrome" service, and a fresh pairing link in $LINK.
Output docs/demo.gif
Set FontSize 18
Set Width 1100
Set Height 600
Set Theme "Catppuccin Mocha"

Type "sol connect '$LINK'"
Sleep 500ms
Enter
Sleep 40s
Type "curl -s localhost:9000/ping.view?f=json | head -c 120"
Enter
Sleep 5s
