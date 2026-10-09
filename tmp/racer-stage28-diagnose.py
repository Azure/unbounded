"""Stage28 artifact and read-only cluster diagnosis; no Kubernetes API writes.

Transport probes use existing listeners only.
"""
import collections
import csv
import gzip
import json
from pathlib import Path
import re
import subprocess
import sys
import urllib.parse

ROOT = Path('/home/azureuser/code/unbounded/tmp')
K = ['kubectl', '--context=joolshev-scale-test', '--request-timeout=25s']
NS = 'unbounded-system'

def load(name):
    p = ROOT / name
    return json.loads(gzip.decompress(p.read_bytes()) if p.suffix == '.gz' else p.read_text())

def run(args):
    p = subprocess.run(['timeout', '--signal=TERM', '--kill-after=10s', '40s', *args], capture_output=True, text=True)
    if p.returncode:
        raise RuntimeError(p.stderr)
    return p.stdout

def get(*args):
    return json.loads(run(K + list(args) + ['-o', 'json']))

def save(name, data):
    (ROOT / ('racer-stage28-' + name + '.json')).write_text(json.dumps(data, indent=2))

def query(q, at=None):
    params = {'query': q}
    if at: params['time'] = at
    return json.loads(run(K + ['get', '--raw', '/api/v1/namespaces/monitoring/services/prometheus:9090/proxy/api/v1/query?' + urllib.parse.urlencode(params)]))

if sys.argv[1] == 'artifacts':
    for name in ['c6-raw.json', 'c6-detail.json', 'c6-plan.json', 'membership.json', 'c6-start-health.json.gz']:
        x = load('racer-stage27-' + name)
        print(name, {k: (list(v)[:30] if isinstance(v, dict) else str(v)[:150]) for k,v in x.items()})
    rows = list(csv.DictReader((ROOT / 'racer-stage27-c6-per-node.csv').open()))
    zeros = [r for r in rows if float(r['verified_GBs']) == 0]
    out = {'zeros': zeros, 'groups': {}, 'rings': {}}
    for pool in sorted({r['pool'] for r in rows}):
        for zero in [False, True]:
            group = [r for r in rows if r['pool'] == pool and (float(r['verified_GBs']) == 0) == zero]
            if group:
                out['groups'][pool + ('-zero' if zero else '-positive')] = {'n': len(group), **{k: sum(float(r[k]) for r in group)/len(group) for k in ['tx_Gbps','rx_Gbps','host_cpu','dataplane_cpu','gantry_cpu','racer-loadgen_cpu','verified_GBs','pull_error']}}
    for node, evidence in load('racer-stage27-failure-evidence.json').items():
        events = [dict(re.findall(r'(\w+)=([^ ]+)', line.split(' detail=')[0]), detail=line.split(' detail=')[-1]) for line in evidence['failures'].splitlines()[1:]]
        requests = collections.defaultdict(list)
        for event in events:
            if event.get('request') != 'none': requests[event['request']].append(event)
        out['rings'][node] = {'counts': dict(collections.Counter(e['stage'] + '/' + e['error'] for e in events)), 'first_ms': min(e['unix_millis'] for e in events), 'last_ms': max(e['unix_millis'] for e in events), 'terminal_correlations': [v for v in requests.values() if any(e['stage'] in ['ClientRead','FirstSlice','ClientDelivery','Metadata'] for e in v)]}
    save('artifact-analysis', out)
    print(json.dumps(out['groups'], indent=2))
    print(json.dumps(out['rings'], indent=2))
elif sys.argv[1] == 'inventory':
    out = {}
    for key, args in {
        'ds': ['-n', NS, 'get', 'ds', 'racer-dataplane', 'gantry', 'racer-loadgen'],
        'services': ['-n', NS, 'get', 'services'],
        'endpoints': ['-n', NS, 'get', 'endpointslices'],
        'pods': ['-n', NS, 'get', 'pods'],
        'volume': ['get', 'clustervolumes'],
    }.items():
        out[key] = get(*args)
    save('inventory', out)
    for d in out['ds']['items']:
        print(d['metadata']['name'], json.dumps(d['spec']['template']['spec']))
    print('services', json.dumps(out['services']))
elif sys.argv[1] == 'historical':
    at = '2026-09-29T10:57:23.818800Z'
    metrics = ['node_netstat_Tcp_RetransSegs', 'node_netstat_TcpExt_TCPSynRetrans', 'node_netstat_TcpExt_TCPTimeouts', 'node_netstat_TcpExt_TCPMTUPFail', 'node_nf_conntrack_entries', 'node_nf_conntrack_entries_limit', 'node_network_mtu_bytes', 'racer_request_errors_total', 'racer_origin_fills_total', 'racer_loadgen_pull_errors_total', 'racer_loadgen_received_bytes_total']
    out = {}
    for metric in metrics:
        q = metric if any(s in metric for s in ['mtu','entries']) else 'increase(' + metric + '[5m])'
        out[metric] = query(q, at)
        data = out[metric].get('data',{}).get('result',[])
        print(metric, len(data), json.dumps(data[:2]))
    save('historical', out)
elif sys.argv[1] == 'hosts':
    import concurrent.futures
    inv = load('racer-stage28-inventory.json')
    zeros = {r['node'] for r in load('racer-stage27-zero-goodput.json')}
    targets = zeros | {'aks-ddsv6-84072342-vmss000023', 'aks-ddsv6-84072342-vmss00000i', 'aks-adsv5-13731677-vmss00007m'}
    pods = {p['spec'].get('nodeName'): p['metadata']['name'] for p in inv['pods']['items'] if p['metadata']['labels'].get('app.kubernetes.io/name') == 'unbounded-net-node'}
    code = '''import json,subprocess,pathlib,stat
def r(args):
 p=subprocess.run(args,capture_output=True,text=True,timeout=8);return {'rc':p.returncode,'stdout':p.stdout,'stderr':p.stderr}
out={k:r(v) for k,v in {'links':['ip','-j','-d','link'], 'routes':['ip','-j','route'], 'rules':['ip','-j','rule'], 'sockets':['ss','-s'], 'nic':['ethtool','-S','eth0']}.items()}
out['proc']={p:pathlib.Path(p).read_text() for p in ['/proc/net/snmp','/proc/net/netstat','/proc/sys/net/netfilter/nf_conntrack_count','/proc/sys/net/netfilter/nf_conntrack_max','/proc/sys/net/ipv4/tcp_mtu_probing'] if pathlib.Path(p).exists()}
out['uds']=[{'path':str(p),'socket':stat.S_ISSOCK(p.stat().st_mode)} for p in pathlib.Path('/run/racer/gantry').glob('*/*')]
print(json.dumps(out))'''
    def inspect(node):
        try:
            result = json.loads(run(K + ['-n',NS,'exec',pods[node],'-c','node','--','chroot','/proc/1/root','/usr/bin/timeout','--signal=TERM','--kill-after=10s','25s','python3','-c',code]))
            return node, result
        except Exception as e: return node, {'error': str(e)}
    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool: out = dict(pool.map(inspect, sorted(targets)))
    save('hosts', out)
    for node, data in out.items():
        print(node, data.get('error') or [(l['ifname'],l['mtu']) for l in json.loads(data['links']['stdout']) if l['ifname'] in ['eth0','eth1']])
elif sys.argv[1] == 'metrics-detail':
    inv = load('racer-stage28-inventory.json')
    zeros = {r['node'] for r in load('racer-stage27-zero-goodput.json')}
    out = {}
    for k, result in load('racer-stage28-historical.json').items():
        out[k] = [r for r in result.get('data',{}).get('result',[]) if r['metric'].get('node') in zeros]
    for service in ['racer-loadgen-gantry','racer-loadgen-origin']:
        eps = [e for s in inv['endpoints']['items'] if s['metadata']['labels'].get('kubernetes.io/service-name') == service for e in s['endpoints']]
        out[service] = {n:[e for e in eps if e.get('nodeName') == n] for n in sorted(zeros)}
    out['membership'] = [p for p in load('racer-stage27-membership.json')['pods'] if p['node'] in zeros]
    save('metrics-detail', out)
    print(json.dumps(out, indent=2))
elif sys.argv[1] == 'host-summary':
    for node, d in load('racer-stage28-hosts.json').items():
        print(node, '\nNIC', d['nic'], '\nSOCKETS', d['sockets'], '\nUDS', d['uds'])
        print('CONNTRACK', {k:v for k,v in d['proc'].items() if 'conntrack' in k})
        print('ROUTES', d['routes']['stdout'])
        print('LINKS', [(x['ifname'], x['mtu'], x.get('linkinfo')) for x in json.loads(d['links']['stdout']) if not x['ifname'].startswith('lxc')])
elif sys.argv[1] == 'path-probes':
    import concurrent.futures
    inv = load('racer-stage28-inventory.json')
    nodes = {p['node']:p['ip'] for p in load('racer-stage27-membership.json')['pods']}
    targets = {r['node'] for r in load('racer-stage27-zero-goodput.json')} | {'aks-ddsv6-84072342-vmss000023'}
    pods = {p['spec'].get('nodeName'):p['metadata']['name'] for p in inv['pods']['items'] if p['metadata']['labels'].get('app.kubernetes.io/name') == 'unbounded-net-node'}
    def inspect(node):
        peers = [nodes['aks-ddsv6-84072342-vmss000023']] if not node.endswith('000023') else [nodes[n] for n in sorted(targets) if n != node]
        code = '''import subprocess,json,time
def r(a):
 try:
  p=subprocess.run(a,capture_output=True,text=True,timeout=6);return {'rc':p.returncode,'out':p.stdout,'err':p.stderr}
 except Exception as e:return {'error':str(e)}
out={'time':time.time(),'clock':r(['chronyc','tracking']),'probes':{}}
for ip in ''' + repr(peers) + ''':
 for size in [1414,1472]:out['probes'][ip+'/'+str(size)]=r(['ping','-n','-c','1','-W','1','-M','do','-s',str(size),ip])
print(json.dumps(out))'''
        try: return node,json.loads(run(K+['-n',NS,'exec',pods[node],'-c','node','--','chroot','/proc/1/root','/usr/bin/timeout','--signal=TERM','--kill-after=10s','30s','python3','-c',code]))
        except Exception as e: return node,{'error':str(e)}
    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool: out=dict(pool.map(inspect,sorted(targets)))
    save('path-probes',out)
    for n,d in out.items():print(n,json.dumps(d))
elif sys.argv[1] == 'vf-history':
    out={}
    for metric in ['node_network_transmit_drop_total','node_network_receive_drop_total','node_network_transmit_bytes_total','node_network_receive_bytes_total']:
        out[metric]=query('increase('+metric+'{job="node-exporter",device="ens1"}[5m])','2026-09-29T10:57:23.818800Z')
        print(metric,len(out[metric]['data']['result']))
    save('vf-history',out)
elif sys.argv[1] == 'origin-transport':
    inv=load('racer-stage28-inventory.json');prefix='aks-ddsv6-84072342-vmss'
    pods={p['spec'].get('nodeName'):p['metadata']['name'] for p in inv['pods']['items'] if p['metadata']['labels'].get('app.kubernetes.io/name')=='unbounded-net-node'}
    origins={p['spec'].get('nodeName'):p['status'].get('podIP') for p in inv['pods']['items'] if p['metadata']['labels'].get('app.kubernetes.io/name')=='racer-loadgen'}
    out=[]
    for source,dest in [('00000i','00005f'),('00005f','00000i'),('00000i','000023'),('000023','00000i')]:
        source=prefix+source;dest=prefix+dest
        code='''import http.client,json,time,socket
c=http.client.HTTPConnection('''+repr(origins[dest])+''',8080,timeout=8)
c.request('GET','/v2/benchmark/image/manifests/latest');r=c.getresponse();manifest=json.loads(r.read());digest=manifest['layers'][0]['digest']
start=time.monotonic();c.request('GET','/v2/benchmark/image/blobs/'+digest,headers={'Range':'bytes=0-16777215'});r=c.getresponse();n=0
while True:
 b=r.read(262144)
 if not b:break
 n+=len(b)
print(json.dumps({'bytes':n,'status':r.status,'seconds':time.monotonic()-start,'tcp_info_hex':c.sock.getsockopt(socket.IPPROTO_TCP,socket.TCP_INFO,256).hex()}));c.close()'''
        try:result={'source':source,'origin':dest,**json.loads(run(K+['-n',NS,'exec',pods[source],'-c','node','--','chroot','/proc/1/root','/usr/bin/timeout','--signal=TERM','--kill-after=10s','20s','python3','-c',code]))}
        except Exception as e:result={'source':source,'origin':dest,'error':str(e)}
        out.append(result);print(json.dumps(result),flush=True)
    save('origin-transport',out)
elif sys.argv[1] == 'vf-history-summary':
    ips={p['ip']:p['node'] for p in load('racer-stage27-membership.json')['pods']}
    out=collections.defaultdict(dict)
    for metric,res in load('racer-stage28-vf-history.json').items():
        for r in res['data']['result']:
            ip=r['metric']['instance'].split(':')[0];n=ips.get(ip,ip)
            out[n][metric]=float(r['value'][1])
    zeros={r['node'] for r in load('racer-stage27-zero-goodput.json')}
    for n,d in out.items():
        if n in zeros or n.endswith(('000023','00000i','0000a0')):print(n,d)
    save('vf-history-summary',out)
elif sys.argv[1] == 'namespace-transport':
    inv=load('racer-stage28-inventory.json');prefix='aks-ddsv6-84072342-vmss'
    pods={p['spec'].get('nodeName'):p['metadata']['name'] for p in inv['pods']['items'] if p['metadata']['labels'].get('app.kubernetes.io/name')=='unbounded-net-node'}
    origins={p['spec'].get('nodeName'):p['status'].get('podIP') for p in inv['pods']['items'] if p['metadata']['labels'].get('app.kubernetes.io/name')=='racer-loadgen'}
    out=[]
    for source,dest in [('00000i','00005f'),('00005f','00000i')]:
        source=prefix+source;dest=prefix+dest
        client='''import http.client,json,time,socket
c=http.client.HTTPConnection('''+repr(origins[dest])+''',8080,timeout=8)
c.request('GET','/v2/benchmark/image/manifests/latest');r=c.getresponse();manifest=json.loads(r.read());digest=manifest['layers'][0]['digest']
start=time.monotonic();c.request('GET','/v2/benchmark/image/blobs/'+digest,headers={'Range':'bytes=0-16777215'});r=c.getresponse();n=0
while True:
 b=r.read(262144)
 if not b:break
 n+=len(b)
print(json.dumps({'bytes':n,'status':r.status,'seconds':time.monotonic()-start,'local':c.sock.getsockname(),'remote':c.sock.getpeername(),'tcp_info_hex':c.sock.getsockopt(socket.IPPROTO_TCP,socket.TCP_INFO,256).hex()}));c.close()'''
        code='''import subprocess,json
pods=json.loads(subprocess.check_output(['crictl','pods','--name','racer-loadgen','-o','json'],timeout=5))['items']
assert len(pods)==1
p=json.loads(subprocess.check_output(['crictl','inspectp',pods[0]['id']],timeout=5))['info']['pid']
subprocess.run(['nsenter','-t',str(p),'-n','python3','-c','''+repr(client)+'''],check=True,timeout=22)'''
        try:result={'source':source,'origin':dest,**json.loads(run(K+['-n',NS,'exec',pods[source],'-c','node','--','chroot','/proc/1/root','/usr/bin/timeout','--signal=TERM','--kill-after=10s','30s','python3','-c',code]))}
        except Exception as e:result={'source':source,'origin':dest,'error':str(e)}
        out.append(result);print(json.dumps(result),flush=True)
    save('namespace-transport',out)
elif sys.argv[1] == 'socket-trace':
    import concurrent.futures
    import time
    inv=load('racer-stage28-inventory.json');prefix='aks-ddsv6-84072342-vmss';source=prefix+'00000i';dest=prefix+'00005f'
    pods={p['spec'].get('nodeName'):p['metadata']['name'] for p in inv['pods']['items'] if p['metadata']['labels'].get('app.kubernetes.io/name')=='unbounded-net-node'}
    ip=next(p['status']['podIP'] for p in inv['pods']['items'] if p['metadata']['labels'].get('app.kubernetes.io/name')=='racer-loadgen' and p['spec']['nodeName']==dest)
    client='''import http.client,json,time
c=http.client.HTTPConnection('''+repr(ip)+''',8080,timeout=8);c.request('GET','/v2/benchmark/image/manifests/latest');r=c.getresponse();m=json.loads(r.read());start=time.monotonic();c.request('GET','/v2/benchmark/image/blobs/'+m['layers'][0]['digest'],headers={'Range':'bytes=0-16777215'});r=c.getresponse();n=0
while True:
 b=r.read(262144)
 if not b:break
 n+=len(b)
print(json.dumps({'bytes':n,'seconds':time.monotonic()-start}));c.close()'''
    inspect='''import subprocess,json
ps=json.loads(subprocess.check_output(['crictl','pods','--name','racer-loadgen','-o','json'],timeout=5))['items'];assert len(ps)==1
pid=json.loads(subprocess.check_output(['crictl','inspectp',ps[0]['id']],timeout=5))['info']['pid']
print(subprocess.check_output(['nsenter','-t',str(pid),'-n','ss','-tin','sport','=',':8080'],text=True,timeout=5))'''
    def remote(node,code):return run(K+['-n',NS,'exec',pods[node],'-c','node','--','chroot','/proc/1/root','/usr/bin/timeout','--signal=TERM','--kill-after=10s','25s','python3','-c',code])
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
        future=pool.submit(remote,source,client);time.sleep(2)
        sockets=remote(dest,inspect);result=future.result()
    save('socket-trace',{'sender':dest,'receiver':source,'result':result,'sockets':sockets});print(sockets,result)
elif sys.argv[1] == 'final-audit':
    import datetime
    out={'at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'ds':get('-n',NS,'get','ds','racer-dataplane','gantry','racer-loadgen'),'deploy':get('-n',NS,'get','deploy','unbounded-operator','racer-controller')}
    for k,q in {'concurrency':'count by (value)(count_values("value",racer_loadgen_applied_concurrency{job="gantry-bench-client"}))','positive':'count(rate(racer_loadgen_verified_bytes_total[2m])>0)','targets':'count(up{job="gantry-bench-client"})'}.items():out[k]=query(q)
    save('final-audit',out)
    print(out['at'])
    for x in out['ds']['items']:print(x['metadata']['name'],x['status'])
    for k in ['concurrency','positive','targets']:print(k,out[k])
elif sys.argv[1] == 'tcp-summary':
    for n,d in load('racer-stage28-hosts.json').items():
        counters={}
        for path in ['/proc/net/snmp','/proc/net/netstat']:
            lines=d['proc'][path].splitlines()
            for i in range(0,len(lines),2):counters.update(dict(zip(lines[i].split()[1:],lines[i+1].split()[1:])))
        print(n,{k:v for k,v in counters.items() if k in ['RetransSegs','OutSegs','InSegs','TCPTimeouts','TCPMTUPFail','TCPMTUPSuccess','TCPSynRetrans','TCPBacklogDrop','ListenDrops','ListenOverflows','TCPRcvQDrop','TCPZeroWindowDrop','TCPAbortOnMemory','TCPMemoryPressures']})
elif sys.argv[1] == 'series':
    zeros='|'.join(r['node'] for r in load('racer-stage27-zero-goodput.json'))
    out={}
    for name,q in {
        'pull_results':'sum by(node,result)(increase(racer_loadgen_pulls_total{node=~"'+zeros+'"}[5m]))',
        'failures':'sum by(node)(increase(racer_request_errors_total{node=~"'+zeros+'"}[5m]))',
        'clock':'node_timex_offset_seconds',
        'sdk_timeouts':'increase(gantry_racer_sdk_queue_timeouts_total[5m])',
        'sdk_rejections':'increase(gantry_racer_sdk_queue_rejections_total[5m])',
        'names':'count by(__name__)({job="gantry-bench-client"})',
    }.items():
        out[name]=query(q,'2026-09-29T10:57:23.818800Z')
        print(name,json.dumps(out[name])[:14000])
    save('series',out)
elif sys.argv[1] == 'qdisc':
    import concurrent.futures
    inv=load('racer-stage28-inventory.json')
    targets={r['node'] for r in load('racer-stage27-zero-goodput.json')} | {'aks-ddsv6-84072342-vmss000023','aks-ddsv6-84072342-vmss00000i','aks-adsv5-13731677-vmss00007m'}
    pods={p['spec'].get('nodeName'):p['metadata']['name'] for p in inv['pods']['items'] if p['metadata']['labels'].get('app.kubernetes.io/name')=='unbounded-net-node'}
    code='''import subprocess,json
def r(a):
 p=subprocess.run(a,capture_output=True,text=True,timeout=5);return {'rc':p.returncode,'out':p.stdout,'err':p.stderr}
out={'qdisc':r(['tc','-s','qdisc','show']),'filters':r(['tc','filter','show','dev','eth0','egress']),'ingress':r(['tc','filter','show','dev','eth0','ingress']),'rules':r(['iptables-save','-t','mangle']),'driver':r(['ethtool','-i','ens1']),'vf':r(['ethtool','-S','ens1'])}
print(json.dumps(out))'''
    def inspect(n):
        try:return n,json.loads(run(K+['-n',NS,'exec',pods[n],'-c','node','--','chroot','/proc/1/root','/usr/bin/timeout','--signal=TERM','--kill-after=10s','30s','python3','-c',code]))
        except Exception as e:return n,{'error':str(e)}
    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:out=dict(pool.map(inspect,sorted(targets)))
    save('qdisc',out)
    for n,d in out.items():print(n,json.dumps({k:v for k,v in d.items() if k!='vf'}))
elif sys.argv[1] == 'qdisc-summary':
    for n,d in load('racer-stage28-qdisc.json').items():
        print(n)
        print('\n'.join(b.split('\n')[0]+' '+b.split('\n')[1] for b in d['qdisc']['out'].split('qdisc ')[1:] if ' root ' in b.split('\n')[0]))
        print('VF',[(k,v) for k,v in re.findall(r'^\s+([^:]+): (\d+)$',d['vf']['out'],re.M) if any(w in k for w in ['drop','err','discard','stop','wake','timeout'])])
elif sys.argv[1] == 'requests':
    zeros='|'.join(r['node'] for r in load('racer-stage27-zero-goodput.json'))
    out={}
    for metric in ['racer_loadgen_requests_total','racer_loadgen_origin_requests_total','racer_loadgen_request_duration_seconds_sum','racer_loadgen_request_duration_seconds_count']:
        out[metric]=query('increase('+metric+'{node=~"'+zeros+'"}[5m])','2026-09-29T10:57:23.818800Z')
    save('requests',out)
    for k,v in out.items():
        print(k)
        for r in v['data']['result']:
            if float(r['value'][1]):print(r['metric']['node'][-6:],{a:b for a,b in r['metric'].items() if a not in ['node','pod','namespace','instance','job']},r['value'][1])
elif sys.argv[1] == 'vf-survey':
    import concurrent.futures
    inv=load('racer-stage28-inventory.json')
    pods={p['spec'].get('nodeName'):p['metadata']['name'] for p in inv['pods']['items'] if p['metadata']['labels'].get('app.kubernetes.io/name')=='unbounded-net-node' and '-ddsv6-' in p['spec'].get('nodeName','')}
    code='''import subprocess,json,time
def r(a):
 p=subprocess.run(a,capture_output=True,text=True,timeout=5);return p.stdout if p.returncode==0 else p.stderr
print(json.dumps({'time':time.time(),'qdisc':r(['tc','-j','-s','qdisc','show','dev','ens1']),'features':r(['ethtool','-k','ens1']),'rings':r(['ethtool','-g','ens1'])}))'''
    def inspect(item):
        n,p=item
        try:return n,json.loads(run(K+['-n',NS,'exec',p,'-c','node','--','chroot','/proc/1/root','/usr/bin/timeout','--signal=TERM','--kill-after=10s','20s','python3','-c',code]))
        except Exception as e:return n,{'error':str(e)}
    with concurrent.futures.ThreadPoolExecutor(max_workers=20) as pool:out=dict(pool.map(inspect,sorted(pods.items())))
    save('vf-survey',out)
    zeros={r['node'] for r in load('racer-stage27-zero-goodput.json')}
    rows=[]
    for n,d in out.items():
        if 'error' in d:print(n,d);continue
        q=json.loads(d['qdisc']);root=next((x for x in q if x.get('root')), {})
        rows.append({'node':n,'zero':n in zeros,'drops':root.get('drops'),'backlog':root.get('backlog'),'qlen':root.get('qlen'),'features':d['features'],'rings':d['rings']})
    save('vf-summary',rows)
    print('nodes',len(rows))
    for r in sorted(rows,key=lambda r:r['drops'] or 0,reverse=True)[:25]:print({k:v for k,v in r.items() if k not in ['features','rings']})
    print('feature groups',collections.Counter(r['features'] for r in rows))
    print('ring groups',collections.Counter(r['rings'] for r in rows))
elif sys.argv[1] == 'queue-detail':
    import concurrent.futures
    inv=load('racer-stage28-inventory.json')
    targets={r['node'] for r in load('racer-stage27-zero-goodput.json')} | {'aks-ddsv6-84072342-vmss000023','aks-ddsv6-84072342-vmss00000i','aks-ddsv6-84072342-vmss0000a0'}
    pods={p['spec'].get('nodeName'):p['metadata']['name'] for p in inv['pods']['items'] if p['metadata']['labels'].get('app.kubernetes.io/name')=='unbounded-net-node'}
    code='''import subprocess,json,pathlib,time
def r(a):
 p=subprocess.run(a,capture_output=True,text=True,timeout=5);return p.stdout if p.returncode==0 else p.stderr
def queues():
 out={}
 for p in pathlib.Path('/sys/class/net/ens1/queues').glob('tx-*/*'):
  if p.is_dir():
   for f in p.iterdir():
    try:out[str(f)]=f.read_text().strip()
    except OSError:pass
  else:
   try:out[str(p)]=p.read_text().strip()
   except OSError:pass
 return out
out={'time':time.time(),'queues':queues(),'vf_before':r(['ethtool','-S','ens1']),'qdisc_before':r(['tc','-j','-s','qdisc','show','dev','ens1']),'interrupts':pathlib.Path('/proc/interrupts').read_text(),'softnet':pathlib.Path('/proc/net/softnet_stat').read_text(),'kernel':r(['journalctl','-k','--since','2026-09-29 10:30:00','--until','2026-09-29 11:02:00','--no-pager','-n','100','--grep=mana|netvsc|watchdog|queue|conntrack'])}
time.sleep(5)
out.update(vf_after=r(['ethtool','-S','ens1']),qdisc_after=r(['tc','-j','-s','qdisc','show','dev','ens1']))
print(json.dumps(out))'''
    def inspect(n):
        try:return n,json.loads(run(K+['-n',NS,'exec',pods[n],'-c','node','--','chroot','/proc/1/root','/usr/bin/timeout','--signal=TERM','--kill-after=10s','30s','python3','-c',code]))
        except Exception as e:return n,{'error':str(e)}
    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:out=dict(pool.map(inspect,sorted(targets)))
    save('queue-detail',out)
    for n,d in out.items():
        if 'error' in d:print(n,d);continue
        a=dict(re.findall(r'^\s+([^:]+): (\d+)$',d['vf_before'],re.M));b=dict(re.findall(r'^\s+([^:]+): (\d+)$',d['vf_after'],re.M))
        delta={k:int(b[k])-int(v) for k,v in a.items() if int(b[k])!=int(v)}
        print(n,'delta',delta,'queues',d['queues'],'kernel',d['kernel'])
elif sys.argv[1] == 'queue-summary':
    out={}
    for n,d in load('racer-stage28-queue-detail.json').items():
        a=dict(re.findall(r'^\s+([^:]+): (\d+)$',d['vf_before'],re.M));b=dict(re.findall(r'^\s+([^:]+): (\d+)$',d['vf_after'],re.M))
        q1=next(x for x in json.loads(d['qdisc_before']) if x.get('root'));q2=next(x for x in json.loads(d['qdisc_after']) if x.get('root'))
        out[n]={'stop_delta':int(b['stop_queue'])-int(a['stop_queue']),'tx_bytes_delta':int(b['hc_tx_bytes'])-int(a['hc_tx_bytes']),'qdisc_drops_delta':q2['drops']-q1['drops'],'backlog':q2['backlog'],'irq': '\n'.join(l for l in d['interrupts'].splitlines() if 'mana' in l.lower() or 'gdma' in l.lower()),'kernel':d['kernel']}
    save('queue-summary',out);print(json.dumps(out,indent=2))
elif sys.argv[1] == 'vf-config':
    import concurrent.futures
    inv=load('racer-stage28-inventory.json')
    targets=set(load('racer-stage28-queue-detail.json'))
    pods={p['spec'].get('nodeName'):p['metadata']['name'] for p in inv['pods']['items'] if p['metadata']['labels'].get('app.kubernetes.io/name')=='unbounded-net-node'}
    code='''import subprocess,json,pathlib
def r(a):
 p=subprocess.run(a,capture_output=True,text=True,timeout=5);return {'rc':p.returncode,'out':p.stdout,'err':p.stderr}
out={k:r(v) for k,v in {'coalesce':['ethtool','-c','ens1'],'channels':['ethtool','-l','ens1'],'rss':['ethtool','-x','ens1'],'irqbalance':['systemctl','is-active','irqbalance'],'sysctl':['sysctl','net.core.netdev_budget','net.core.netdev_budget_usecs','net.ipv4.tcp_congestion_control']}.items()}
out['irq']={str(p):p.read_text().strip() for i in range(34,42) for p in [pathlib.Path('/proc/irq')/str(i)/'effective_affinity_list'] if p.exists()}
print(json.dumps(out))'''
    def inspect(n):
        try:return n,json.loads(run(K+['-n',NS,'exec',pods[n],'-c','node','--','chroot','/proc/1/root','/usr/bin/timeout','--signal=TERM','--kill-after=10s','30s','python3','-c',code]))
        except Exception as e:return n,{'error':str(e)}
    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:out=dict(pool.map(inspect,sorted(targets)))
    save('vf-config',out)
    for n,d in out.items():print(n,json.dumps(d))
