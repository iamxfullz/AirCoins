#!/bin/bash
# Temporary validation script for the GPIO master toggle changes
ok=1
for f in install.sh zerotier-install system/usr/local/bin/gpio-coin-listener \
         system/usr/lib/cgi-bin/set_gpio_config system/usr/lib/cgi-bin/get_gpio_config; do
    if bash -n "$f" 2>err.txt; then
        echo "SYNTAX OK: $f"
    else
        echo "SYNTAX FAIL: $f"; cat err.txt; ok=0
    fi
done
rm -f err.txt

# --- Live test: set_gpio_config partial update (master toggle alone) ---
mkdir -p /var/lib/pisowifi 2>/dev/null
cat > /var/lib/pisowifi/gpio_config << 'EOF'
GPIO_ENABLED=true
COIN_PULSE_PIN=11
COIN_VALUE=1
PULSE_MODE="rising"
BOARD_MODEL="Orange Pi PC"
DEBOUNCE_MS=30
EOF

OUT=$(echo '{"gpio_enabled":false}' | bash system/usr/lib/cgi-bin/set_gpio_config 2>/dev/null | tail -1)
echo "PARTIAL POST RESPONSE: $OUT"
echo "$OUT" | node -e "let d='';process.stdin.on('data',c=>d+=c).on('end',()=>{const j=JSON.parse(d);if(j.status==='ok'&&j.gpio_enabled===false&&j.pin===11){console.log('PARTIAL UPDATE TEST: PASS (pin preserved, toggle off)')}else{console.log('PARTIAL UPDATE TEST: FAIL',JSON.stringify(j));process.exit(1)}})" || ok=0

# Config file must keep the preserved pin and gain GPIO_ENABLED=false
if grep -q 'COIN_PULSE_PIN=11' /var/lib/pisowifi/gpio_config && grep -q 'GPIO_ENABLED=false' /var/lib/pisowifi/gpio_config; then
    echo "CONFIG FILE TEST: PASS (pin preserved, toggle written)"
else
    echo "CONFIG FILE TEST: FAIL"; cat /var/lib/pisowifi/gpio_config; ok=0
fi

# --- Live test: get_gpio_config (JSON output) ---
OUT2=$(bash system/usr/lib/cgi-bin/get_gpio_config 2>/dev/null | tail -1)
echo "READ RESPONSE: $OUT2"
echo "$OUT2" | node -e "let d='';process.stdin.on('data',c=>d+=c).on('end',()=>{const j=JSON.parse(d);if(j.status==='ok'&&j.gpio_enabled===false&&j.pin===11&&typeof j.gpio_method!=='undefined'){console.log('READ CGI TEST: PASS')}else{console.log('READ CGI TEST: FAIL',JSON.stringify(j));process.exit(1)}})" || ok=0

# --- Restore default test config ---
echo "GPIO_ENABLED=true" > /var/lib/pisowifi/gpio_config

[ "$ok" = "1" ] && echo "ALL TESTS PASSED" || echo "SOME TESTS FAILED"
exit $((1-ok))