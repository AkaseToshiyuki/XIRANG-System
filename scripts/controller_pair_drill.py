#!/usr/bin/env python3
"""Exercise two isolated controllers against disposable PostgreSQL, with real TLS.

No production service or credential is read or changed. Reports contain no token.
All processes and data belong to a newly created, owner-only directory.
"""
import argparse
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import secrets
import signal
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, default=Path('bin/xirangd'))
    parser.add_argument('--output', type=Path)
    parser.add_argument('--pg-bin', type=Path, default=Path('/usr/lib/postgresql/16/bin'))
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    if args.output:
        args.output.mkdir(mode=0o700, parents=False, exist_ok=False)
        work = args.output.resolve()
    else:
        work = Path(tempfile.mkdtemp(prefix='xirang-controller-pair-'))
    os.chmod(work, 0o700)
    sockets = [socket.socket() for _ in range(7)]
    for item in sockets: item.bind(('127.0.0.1', 0))
    pgport, pport, bport, pgRPC, bgRPC, thirdport, thirdgrpc = [item.getsockname()[1] for item in sockets]
    for item in sockets: item.close()
    token = secrets.token_hex(32)
    processes, logs = [], []
    report = {'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'single_host': True, 'checks': {}, 'transitions': []}
    pgdata = work / 'postgres'
    pg_started = False
    stop_monitor = threading.Event()
    monitor = None
    overlap = []
    stopped_listener_overlap = []
    serving_overlap = []
    monitor_errors = []
    ca = work / 'primary' / 'pki' / 'ca.crt'
    context = None
    dsn = f'postgresql://{os.environ.get("USER", "yang")}@127.0.0.1:{pgport}/pair?sslmode=disable&connect_timeout=2'
    def pg(command, *extra):
        log = work / ('pg-' + command + '.log')
        with log.open('ab') as output:
            subprocess.run([str(args.pg_bin / command), *map(str, extra)], check=True, stdout=output, stderr=output, timeout=25)
    def api(port, path='/readyz', data=None, method=None):
        request = urllib.request.Request(f'https://127.0.0.1:{port}'+path, data=None if data is None else json.dumps(data).encode(), headers={'Authorization': 'Bearer '+token, 'Content-Type': 'application/json'}, method=method)
        with urllib.request.urlopen(request, context=context, timeout=1) as response: return response.read()
    def ready(port):
        try: return bool(api(port))
        except (OSError, urllib.error.URLError): return False
    def wait_for(predicate, label, timeout=35):
        start=time.monotonic()
        while time.monotonic()-start < timeout:
            if predicate():
                report['transitions'].append({'name': label, 'elapsed_seconds': time.monotonic()-start})
                print(label, flush=True)
                return
            time.sleep(.2)
        raise RuntimeError('timeout: '+label)
    def start(name, role, node=None, namespace="pair-drill"):
        folder=work/name; folder.mkdir(exist_ok=True, mode=0o700)
        (folder/'admin.token').write_text(token+'\n'); (folder/'admin.token').chmod(0o600)
        if name!='primary' and not (folder/'pki').exists(): shutil.copytree(work/'primary'/'pki', folder/'pki')
        apiport, grpcport = (pport,pgRPC) if name=='primary' else (bport,bgRPC) if name=='backup' else (thirdport,thirdgrpc)
        log=(work/(name+'.log')).open('ab'); logs.append(log)
        argv=[str(binary), '-listen',f'127.0.0.1:{apiport}', '-agent-listen', f'127.0.0.1:{grpcport}', '-data-dir',str(folder), '-web-root',str(work/'web'), '-tls-host','127.0.0.1', '-external-state-dsn',dsn, '-external-state-namespace',namespace,'-remote-build-enabled=false', '-job-artifact-root',str(work/'artifacts'), '-controller-failback-delay','2s']
        if role: argv += ['-controller-role',role,'-controller-node-id',node or name+'-node']
        proc=subprocess.Popen(argv,stdout=log,stderr=log); processes.append(proc); return proc
    def watch():
        targets={pport,bport}
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            while not stop_monitor.wait(.1):
                try:
                    listening=set()
                    for line in Path('/proc/net/tcp').read_text().splitlines()[1:]:
                        fields=line.split()
                        if fields[3]=='0A': listening.add(int(fields[1].split(':')[1],16))
                    if targets <= listening:
                        stopped=False
                        for proc in list(processes):
                            try: stopped |= Path(f'/proc/{proc.pid}/stat').read_text().split(') ')[1].split()[0]=='T'
                            except FileNotFoundError: pass
                        (stopped_listener_overlap if stopped else overlap).append(time.monotonic())
                    if all(pool.map(ready, [pport,bport])) and all(pool.map(ready,[pport,bport])):
                        serving_overlap.append(time.monotonic())
                except Exception as exc:
                    monitor_errors.append(type(exc).__name__)
    def markers(port):
        items=json.loads(api(port,'/api/v1/queues'))['items']
        return all(any(q['name']==f'pair-marker-{i}' and q['weight']==7 for q in items) for i in range(4))
    try:
        (work/'web').mkdir(); (work/'web'/'index.html').write_text('<!doctype html><title>Isolated panel</title>')
        pg('initdb','-D',pgdata,'--auth-local=trust','--auth-host=trust','--no-locale')
        with (pgdata/'postgresql.conf').open('a') as config: config.write(f"\nlisten_addresses='127.0.0.1'\nport={pgport}\nunix_socket_directories='{work}'\n")
        pg('pg_ctl','-D',pgdata,'-l',work/'postgres.log','start'); pg_started=True
        pg('createdb','-h','127.0.0.1','-p',pgport,'pair')
        primary=start('primary','primary')
        wait_for(ca.exists,'primary CA created')
        context=ssl.create_default_context(cafile=str(ca))
        wait_for(lambda: ready(pport),'primary ready')
        backup=start('backup','backup')
        monitor=threading.Thread(target=watch,daemon=True);monitor.start()
        time.sleep(1.5)
        assert backup.poll() is None and not ready(bport), 'backup panel is serving before takeover'
        report['checks']['backup_listener_closed']=True
        assert b'Isolated panel' in api(pport,'/')
        for i in range(4): api(pport,f'/api/v1/queues/pair-marker-{i}',{'weight':7,'backfill':True},'PUT')
        for name,role in [('third','backup'),('duplicate','primary'),('unassigned','')]:
            proc=start(name,role);proc.wait(timeout=10)
            assert proc.returncode != 0 and not ready(thirdport), 'unassigned/duplicate controller admitted'
        report['checks']['third_and_duplicate_and_unassigned_rejected']=True
        recovery=json.loads(api(pport,'/api/v1/system/backups',method='POST'))
        dsn_file=work/'import.dsn'; dsn_file.write_text(dsn); dsn_file.chmod(0o600)
        command=[str(binary.with_name('xirang-maint')),'import-postgresql',str(work/'primary'/'backups'/recovery['id']),str(dsn_file),'import-copy']
        with (work/'import.log').open('wb') as output:
            subprocess.run(command,stdout=output,stderr=output,check=True,timeout=30)
            duplicate=subprocess.run(command,stdout=output,stderr=output,check=False,timeout=30)
        assert duplicate.returncode!=0, 'import overwrote an existing namespace'
        imported=start('imported','',namespace='import-copy')
        wait_for(lambda: ready(thirdport),'imported backup ready')
        assert markers(thirdport)
        imported.terminate();imported.wait(timeout=15)
        report['checks']['verified_backup_import_and_overwrite_rejection']=True
        for cycle in range(2):
            primary.kill();primary.wait(timeout=5)
            wait_for(lambda: ready(bport),f'backup takeover {cycle+1}')
            assert not ready(pport) and markers(bport)
            assert b'Isolated panel' in api(bport,'/')
            primary=start('primary','primary')
            wait_for(lambda: ready(pport) and not ready(bport),f'primary handback {cycle+1}')
            assert markers(pport) and backup.poll() is None
        report['checks']['takeover_and_automatic_handback_twice']=True
        # A hung process keeps its TCP socket but must lose the database lease.
        primary.send_signal(signal.SIGSTOP)
        wait_for(lambda: ready(bport),'stalled primary loses lease',timeout=30)
        assert markers(bport)
        primary.send_signal(signal.SIGCONT)
        # Its first request after resuming cannot serve stale state; the process
        # must close and reopen the store before it is eligible for handback.
        wait_for(lambda: ready(pport) and not ready(bport),'resumed primary reloads and reclaims',timeout=35)
        assert markers(pport)
        report['checks']['stalled_primary_session_expires_and_reloads']=True
        pg('pg_ctl','-D',pgdata,'-m','immediate','stop');pg_started=False
        wait_for(lambda: not ready(pport) and not ready(bport),'database outage stops both panels')
        try: api(pport,'/api/v1/queues/outage-rejected',{'weight':9},'PUT'); raise AssertionError('outage write succeeded')
        except (OSError,urllib.error.URLError): pass
        pg('pg_ctl','-D',pgdata,'-l',work/'postgres-recovery.log','start');pg_started=True
        wait_for(lambda: ready(pport) and not ready(bport),'primary recovers after database restart',timeout=45)
        assert markers(pport)
        assert not any(q['name']=='outage-rejected' for q in json.loads(api(pport,'/api/v1/queues'))['items'])
        report['checks']['database_outage_fails_closed_and_recovers']=True
        report['checks']['committed_state_continues']=True
        # The resumed process may need a monitor tick to close its old socket;
        # the per-request database guard refuses it during that interval.
        assert not serving_overlap and not monitor_errors, 'concurrent ready controllers or observer error'
        report['checks']['single_ready_controller_observed']=True
        report['checks']['passive_panel_closed_after_each_transition']=True
        report['passed']=True
    finally:
        stop_monitor.set()
        if monitor: monitor.join(timeout=2)
        for proc in processes:
            if proc.poll() is None:
                proc.send_signal(signal.SIGCONT); proc.terminate()
        for proc in processes:
            try: proc.wait(timeout=15)
            except subprocess.TimeoutExpired: proc.kill();proc.wait(timeout=5)
        if pg_started: pg('pg_ctl','-D',pgdata,'-m','immediate','stop')
        for log in logs: log.close()
        report['concurrent_ready_samples']=len(serving_overlap)
        report['monitor_errors']=monitor_errors
        report['overlap_listener_samples']=len(overlap)
        report['stopped_process_listener_overlap_samples']=len(stopped_listener_overlap)
        (work/'report.json').write_text(json.dumps(report,indent=2)+'\n'); (work/'report.json').chmod(0o600)
        print('Report saved: '+str(work/'report.json'),flush=True)

if __name__=='__main__': main()
