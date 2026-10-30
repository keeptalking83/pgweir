# Records the README GIF: one run of examples/conflict_demo.sh.
#
# Needs vhs (https://github.com/charmbracelet/vhs), pgweir and psql on PATH,
# psycopg, and a fresh database:
#   docker compose -f examples/docker-compose.yml down -v
#   docker compose -f examples/docker-compose.yml up -d --wait
# Run from the repository root: vhs examples/demo.tape
Output docs/demo.gif

Set Shell "bash"
Set FontSize 13
Set Width 1280
Set Height 820
Set Padding 16
Set TypingSpeed 35ms
Env DEMO_PAUSE "2.5"

Type "./examples/conflict_demo.sh"
Sleep 400ms
Enter
Wait@120s
Sleep 4s
