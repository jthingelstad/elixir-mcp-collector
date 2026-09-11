# <Platform>

<One sentence: what it costs, how you get a stable outbound IPv4, what
architecture, what supervises the process.> If it is a Linux VM with
cloud-init, link [the cloud VM recipe](cloud-init.md) and write only
the delta: where the user-data box is, how the static IP is reserved,
and anything that surprised you.

## Steps

<Commands, in the order you ran them. Exact console paths (Menu →
Submenu → Button) age well; screenshots do not.>

## Things that surprise people

<The two or three things you had to look up.>

End by running `collector doctor` (or `python3 collector.py --check`)
and pasting its output with the secrets it already redacts. Then open a
pull request adding this file and a row in `README.md`'s table.
