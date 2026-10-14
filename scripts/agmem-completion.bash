# bash completion for agmem (macOS bash 3.2 compatible)
# Flags verified against cmd/agmem/main.go.
# 事实源：本文件随 agmem 二进制内嵌发布（`agmem completion bash` 原样输出）。
# 安装：agmem completion bash > ~/.local/share/bash-completion/completions/agmem
#   或：source <(agmem completion bash)
#
# 说明：agmemd 无命令行 flag（全部走环境变量），故不提供 agmemd 补全。

_agmem_kbs() {
  # 常用知识库列表（轻量：本机已知 kb；服务端查询太慢不适合补全路径）
  echo "sre"
}

_agmem() {
  local cur prev cword cmd
  COMPREPLY=()
  cur="${COMP_WORDS[COMP_CWORD]}"
  cword=$COMP_CWORD

  if [ "$cword" -eq 1 ]; then
    COMPREPLY=( $(compgen -W "push ask status kb forget retry clear completion help --help -h" -- "$cur") )
    return 0
  fi

  cmd="${COMP_WORDS[1]}"

  # 当前词前若紧跟需要值的 flag，则不做 flag 补全
  prev="${COMP_WORDS[cword-1]}"
  case "$prev" in
    --kb) COMPREPLY=( $(compgen -W "$(_agmem_kbs)" -- "$cur") ); return 0;;
    --dir|--id|--name|--task|--k|--max-len|--f|--t) return 0;;
  esac

  case "$cmd" in
    ask)
      COMPREPLY=( $(compgen -W "--kb --dir --global --k --max-len --json --debug --help -h" -- "$cur") )
      return 0;;
    push)
      COMPREPLY=( $(compgen -W "--kb --dir --f --t --help -h" -- "$cur") )
      return 0;;
    status)
      COMPREPLY=( $(compgen -W "--kb --task --help -h" -- "$cur") )
      return 0;;
    kb)
      if [ "$cword" -eq 2 ]; then
        COMPREPLY=( $(compgen -W "create dir list dirs help" -- "$cur") )
        return 0
      fi
      case "${COMP_WORDS[2]}" in
        create) COMPREPLY=( $(compgen -W "--name --help -h" -- "$cur") );;
        dir)    COMPREPLY=( $(compgen -W "--kb --name --help -h" -- "$cur") );;
        dirs)   COMPREPLY=( $(compgen -W "--kb --help -h" -- "$cur") );;
        *)      COMPREPLY=( $(compgen -W "--help -h" -- "$cur") );;
      esac
      return 0;;
    forget)
      COMPREPLY=( $(compgen -W "--id --yes --kb --help -h" -- "$cur") )
      return 0;;
    retry)
      COMPREPLY=( $(compgen -W "--id --kb --help -h" -- "$cur") )
      return 0;;
    clear)
      COMPREPLY=( $(compgen -W "--id --all --json --help -h" -- "$cur") )
      return 0;;
    completion)
      if [ "$cword" -eq 2 ]; then
        COMPREPLY=( $(compgen -W "bash zsh" -- "$cur") )
        return 0
      fi
      COMPREPLY=( $(compgen -W "--help -h" -- "$cur") )
      return 0;;
    *)
      COMPREPLY=( $(compgen -W "--help -h" -- "$cur") )
      return 0;;
  esac
}
complete -F _agmem agmem
