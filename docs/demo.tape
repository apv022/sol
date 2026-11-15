# Record the README demo GIF with https://github.com/charmbracelet/vhs :
#   vhs docs/demo.tape
# Prepare first: `sol` running here and on a machine called "home" that has a
# "navidrome" service, and a fresh pairing link from "home" (`sol invite`) in $LINK.
# On "home", share it once the pairing below is done: sol share <this machine> navidrome
Output docs/demo.gif
Set FontSize 18
Set Width 1100
Set Height 600
Set Theme "Catppuccin Mocha"

Type "sol pair '$LINK'"
Sleep 500ms
Enter
Sleep 40s
Type "sol peers -r"
Enter
Sleep 15s
Type "sol open home"
Enter
Sleep 30s
Type "curl -s localhost:9000/ping.view?f=json | head -c 120"
Enter
Sleep 5s
Type "sol send home notes.pdf"
Enter
Sleep 20s
