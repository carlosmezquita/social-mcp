#!/usr/bin/env bash
set -euo pipefail
bridge=br-social-waha
chain=SOCIAL_WAHA_EGRESS

# Keep active protections in place when rerun. Rules for an isolated bridge only.
iptables -w -L DOCKER-USER -n >/dev/null
ensure_rule() {
  local parent=$1
  shift
  iptables -w -C "$parent" "$@" 2>/dev/null || iptables -w -A "$parent" "$@"
}
iptables -w -L "$chain" -n >/dev/null 2>&1 || iptables -w -N "$chain"
ensure_rule "$chain" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
for network in 0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.0.2.0/24 192.168.0.0/16 198.18.0.0/15 198.51.100.0/24 203.0.113.0/24 224.0.0.0/3; do
  ensure_rule "$chain" -d "$network" -j REJECT
done
iptables -w -C DOCKER-USER -i "$bridge" -j "$chain" 2>/dev/null || iptables -w -I DOCKER-USER 1 -i "$bridge" -j "$chain"
iptables -w -C INPUT -i "$bridge" -m conntrack --ctstate NEW -j REJECT 2>/dev/null || iptables -w -I INPUT 1 -i "$bridge" -m conntrack --ctstate NEW -j REJECT
# An explicit forwarding/host rule protects against later IPv6 configuration changes.
ip6tables -w -C FORWARD -i "$bridge" -j REJECT 2>/dev/null || ip6tables -w -I FORWARD 1 -i "$bridge" -j REJECT
ip6tables -w -C INPUT -i "$bridge" -j REJECT 2>/dev/null || ip6tables -w -I INPUT 1 -i "$bridge" -j REJECT
