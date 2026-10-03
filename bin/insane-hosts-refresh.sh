#!/bin/bash
# Refresh /etc/hosts with real public IPs for insane-search scan targets.
# The sandbox DNS sinkholes everything to 198.18.x.x; the insane engine
# fail-closes on that by design. Real IPs come via DNS-over-HTTPS through
# the egress proxy. TLS certificate verification in the engine still
# authenticates every connection, so DNS is only used for routing.
# /etc/hosts is ephemeral: run this before every scan (and after VM replacement).
set -u
DOMAINS="linux.do www.nodeloc.com nodeloc.com gall.dcinside.com www.v2ex.com v2ex.com"
PROXY="http://hatch-egress-proxy:3128"
DOH="https://cloudflare-dns.com/dns-query"
MARK_BEGIN="# BEGIN insane-search-hosts"
MARK_END="# END insane-search-hosts"

entries=""
for h in $DOMAINS; do
  ips=$(curl -s --max-time 15 -x "$PROXY" "$DOH?name=$h&type=A" -H "accept: application/dns-json" \
    | python3 -c "import json,sys; d=json.load(sys.stdin); print(' '.join(a['data'] for a in d.get('Answer',[]) if a.get('type')==1))" 2>/dev/null)
  if [ -n "$ips" ]; then
    for ip in $ips; do
      entries="$entries$ip $h\n"
    done
    echo "resolved $h -> $ips"
  else
    echo "FAILED to resolve $h" >&2
  fi
done

if [ -z "$entries" ]; then
  echo "no resolutions; leaving /etc/hosts unchanged" >&2
  exit 1
fi

# Remove old managed block, append fresh one.
tmp=$(mktemp)
awk -v b="$MARK_BEGIN" -v e="$MARK_END" '
  $0==b {skip=1; next} $0==e {skip=0; next} !skip {print}
' /etc/hosts > "$tmp"
{
  echo "$MARK_BEGIN"
  printf "%b" "$entries"
  echo "$MARK_END"
} >> "$tmp"
cat "$tmp" > /etc/hosts
rm -f "$tmp"
echo "hosts updated"
