#!/usr/bin/env python3
"""Compare VideoToolbox H.264/HEVC formats and tuning with synthetic frames."""
import argparse, json, os, random, subprocess, time
from pathlib import Path
p=argparse.ArgumentParser()
p.add_argument('binary'); p.add_argument('output')
p.add_argument('--width',type=int,default=1280);p.add_argument('--height',type=int,default=720)
p.add_argument('--fps',type=int,default=60);p.add_argument('--frames',type=int,default=150)
p.add_argument('--repeat',type=int,default=2)
p.add_argument('--focus',action='store_true',help='repeat promising formats and HEVC speed priority')
a=p.parse_args()
results=[]
for repeat in range(a.repeat):
    modes=[(codec,pixel,low,delay) for codec in ['h264','hevc']
           for pixel in ['nv12','bgra','i420','bgra_nv12'] for low,delay in [(0,-1),(1,-1)]]
    modes += [('hevc','nv12',0,1),('hevc','nv12',1,1)]
    modes=[(*m,0) for m in modes]
    if a.focus:
        modes=[('h264','nv12',1,-1,0),('h264','bgra',1,-1,0),('h264','i420',1,-1,0),
               ('h264','bgra_nv12',1,-1,0),('hevc','nv12',0,-1,0),('hevc','nv12',0,-1,1),
               ('hevc','bgra',0,-1,1),('hevc','i420',0,-1,1)]
    random.Random(20261003+repeat).shuffle(modes)
    for codec,pixel,low,delay,speed in modes:
        cmd=[a.binary,str(a.width),str(a.height),str(a.fps),str(a.frames),str(low),str(delay),'baseline','moving',codec,pixel]
        started=time.time()
        try:
            r=subprocess.run(cmd,capture_output=True,text=True,timeout=30,env=dict(os.environ,ANYKVM_VT_SPEED=str(speed)))
            entry={'repeat':repeat+1,'speed_priority':speed,'codec':codec,'pixel_format':pixel,'low':low,'delay':delay,'exit':r.returncode,
                   'result':json.loads(r.stdout) if r.stdout else None,'stderr':r.stderr,'started_unix':started,'command':cmd}
        except subprocess.TimeoutExpired:
            entry={'repeat':repeat+1,'speed_priority':speed,'codec':codec,'pixel_format':pixel,'low':low,'delay':delay,'error':'timeout','command':cmd}
        results.append(entry)
        Path(a.output).write_text(json.dumps(results,indent=2)+'\n')
        print(json.dumps(entry),flush=True)
