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
test('form produces typed recurring product presets and prohibits one-off fixed addresses', () => {
  const elements = Object.fromEntries(['config-form','config-output','copy-button','status','config-kind'].map(id => [id, {value: id==='config-kind'?'preset':'',dataset:{},events:{},addEventListener(name,fn){this.events[name]=fn;}}]));
  const input = {...defaults,network_mode:'dedicated',dedicated_mode:'routed',agent_id:'3',network_bridge:'eth0',network_dns:'1.1.1.1',security_group_ids:'3,9'};
  const context=vm.createContext({URL,document:{getElementById(id){return elements[id];}},FormData:class {constructor(){return Object.entries(input)}}});
  vm.runInContext(html.match(/<script id="config-script">([\s\S]*?)<\/script>/)[1],context);
  elements['config-form'].events.submit({preventDefault(){}});
  const output=JSON.parse(elements['config-output'].value);
  assert.equal(output.mode,'fixed');
  assert.deepEqual(output.cpu,{min:1,max:1,step:0,unit_price_cents:0});
  assert.deepEqual(output.security_group_ids,[3,9]);
  assert.deepEqual(output.network_dns,['1.1.1.1']);
  assert.equal(output.network_mode,'dedicated');
  assert.equal(output.dedicated_mode,'routed');
  assert.equal(output.agent_id,3);
  input.ip_pool_entry_id='11';
  elements['config-form'].events.submit({preventDefault(){}});
  assert.equal(elements['config-output'].value,'');
  assert.equal(elements.status.dataset.error,'true');
  assert.match(elements.status.textContent,/一次性|池条目/);
  for(const name of ['dedicated_mode','security_group_ids']) assert.match(html,new RegExp(`name="${name}"`));
});
test('structured dedicated preset forwards mode and canonical security groups', () => {
  const options=build({...defaults,network_mode:'dedicated',dedicated_mode:'routed',network_bridge:'eth0',security_group_ids:'3,9'});
  assert.equal(options.dedicated_mode,'routed');
  assert.equal(options.security_group_ids,'3,9');
  for(const patch of [{network_mode:'nat',dedicated_mode:'routed'},{network_mode:'dedicated',dedicated_mode:'evil'},{security_group_ids:'03'},{security_group_ids:'3,3'},{security_group_ids:'3, 9'}]) assert.throws(()=>build({...defaults,...patch}));
});
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
