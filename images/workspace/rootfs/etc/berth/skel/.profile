# ~/.profile: executed by login shells (ttyd starts "bash -l").
# Seeded from the image into a new /home/user volume; edit freely, it is yours.

# bash: pull in ~/.bashrc so login and non-login shells look the same
if [ -n "$BASH_VERSION" ] && [ -f "$HOME/.bashrc" ]; then
    . "$HOME/.bashrc"
fi

# per-user bin directories, if they exist
for d in "$HOME/.local/bin" "$HOME/bin"; do
    [ -d "$d" ] && case ":$PATH:" in *":$d:"*) ;; *) PATH="$d:$PATH" ;; esac
done
export PATH
