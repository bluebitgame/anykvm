import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {runInNewContext} from 'node:vm';

test('all video codecs use the shared buffered presenter and reset across codec switches', () => {
  const source=readFileSync(new URL('../web/app.js',import.meta.url),'utf8');
  const helper=source.slice(source.indexOf("let presenterCodec = 'h264';"),source.indexOf('const h264Presenter ='));
  const events=[];
  const frame={timestamp:1234};
  runInNewContext(helper+`
    presentVideoFrame(frame,'h264',1000);
    presentVideoFrame(frame,'hevc');
    presentVideoFrame(frame,'hevc');
    presentVideoFrame(frame,'av1');
  `,{frame,h264Presenter:{reset(){events.push('reset')},push(frame,timestamp){events.push(timestamp)}}});
  assert.deepEqual(events,[1000,'reset',1234,1234,'reset',1234]);
  for(const codec of ['h264','hevc','av1'])assert.ok(source.includes("presentVideoFrame(frame, '"+codec+"'"));
});
