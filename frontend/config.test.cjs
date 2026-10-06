'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const html = fs.readFileSync(require('node:path').join(__dirname, 'index.html'), 'utf8');
function build(input) {
  const script = html.match(/<script id="config-script">([\s\S]*?)<\/script>/);
  assert.ok(script, 'product option builder must exist');
  const context = vm.createContext({URL});
  vm.runInContext(script[1], context);
  return JSON.parse(JSON.stringify(context.buildProductOptions(input)));
}
const defaults = {driver:'incus', cpu:'1', memory_mb:'512', disk_gb:'10', bandwidth_mbps:'0', traffic_gb:'0', network_mode:'nat'};
test('builds complete typed string options with network IDs and unchanged 64-bit IDs', () => {
  const options = build({...defaults, agent_id:'18446744073709551615', vpc_id:'9', network_mode:'vpc', network_ipv4:'10.0.0.7/24', network_gateway:'10.0.0.1', network_dns:'1.1.1.1,2606:4700:4700::1111'});
  assert.equal(options.agent_id, '18446744073709551615');
  assert.equal(options.vpc_id, '9');
  assert.equal(options.network_mode, 'vpc');
  assert.equal(options.network_ipv4, '10.0.0.7/24');
  assert.equal(options.traffic_gb, '0');
  assert.equal(typeof options.memory_mb, 'string');
});
test('dedicated IP pool options do not silently change selected address', () => {
  const options=build({...defaults,agent_id:'3',ip_pool_entry_id:'11',network_mode:'dedicated'});
  assert.equal(options.ip_pool_entry_id,'11');
  assert.equal(options.network_mode,'dedicated');
  assert.equal(options.network_ipv4,undefined);
});
test('blocks unsafe or inconsistent configuration instead of normalizing it', () => {
  for (const patch of [{agent_id:'01'}, {agent_id:'../3'}, {agent_id:'18446744073709551616'}, {driver:'evil'}, {cpu:'NaN'}, {cpu:'65'}, {memory_mb:'0'}, {disk_gb:'1.5'}, {network_mode:'vpc'}, {vpc_id:'9',network_mode:'vpc'}, {agent_id:'3',vpc_id:'9',ip_pool_entry_id:'11',network_mode:'vpc'}, {network_ipv4:'999.0.0.7/24'}, {network_gateway:'evil'}, {network_dns:'javascript:alert(1)'}, {network_bridge:'../br0'}, {network_mac:'not-a-mac'}, {bandwidth_mbps:'100001'}, {api_key:'secret'}, {url:'http://evil'}, {network_mode:'none',network_ipv4:'10.0.0.1'}]) {
    assert.throws(()=>build({...defaults,...patch}),JSON.stringify(patch));
  }
});
test('page never injects output as markup, sends credentials, or persists drafts', () => {
  assert.doesNotMatch(html, /\.innerHTML|\bfetch\s*\(|XMLHttpRequest|localStorage|postMessage|<script[^>]*src=/);
  assert.match(html, /prefers-reduced-motion/);
  assert.match(html, /离线/);
  assert.match(html, /不会.*保存/);
  for (const key of ['vpc_id','ip_pool_entry_id','network_mode','network_bridge','network_ipv4','network_gateway','network_dns','network_mac']) assert.match(html,new RegExp(`name="${key}"`));
});
