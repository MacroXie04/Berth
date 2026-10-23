# ~/.bashrc: executed by interactive bash shells.
# Seeded from the image into a new /home/user volume; edit freely, it is yours.

# nothing to do for non-interactive shells
case $- in
    *i*) ;;
    *) return ;;
esac

# history: big, shared between terminals, no duplicates
HISTCONTROL=ignoreboth:erasedups
HISTSIZE=10000
HISTFILESIZE=20000
shopt -s histappend checkwinsize

# runs before every prompt: flush history, remember a failing exit status
__berth_precmd() {
    local rc=$?
    history -a
    __berth_err=
    [ "$rc" -ne 0 ] && __berth_err="!$rc "
}
PROMPT_COMMAND="__berth_precmd${PROMPT_COMMAND:+; $PROMPT_COMMAND}"

# prompt: user@berth:dir$  (green user, blue dir; red "!N" after a failed command)
PS1='\[\033[31m\]${__berth_err}\[\033[01;32m\]\u@berth\[\033[0m\]:\[\033[01;34m\]\w\[\033[0m\]\$ '

# colours and a few conveniences
if command -v dircolors >/dev/null 2>&1; then
    eval "$(dircolors -b)"
    alias ls='ls --color=auto'
    alias grep='grep --color=auto'
fi
alias ll='ls -alF'
alias la='ls -A'
alias l='ls -CF'

# programmable completion (bash-completion is installed in the image)
if ! shopt -oq posix && [ -f /usr/share/bash-completion/bash_completion ]; then
    . /usr/share/bash-completion/bash_completion
fi

export EDITOR="${EDITOR:-vim}"
export PAGER="${PAGER:-less}"
