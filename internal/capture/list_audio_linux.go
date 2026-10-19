//go:build linux && cgo

package capture

/*
#cgo LDFLAGS: -lasound
#include <alsa/asoundlib.h>
#include <stdlib.h>
#include <string.h>
static char *bk_alsa_inputs(void) {
 void **hints=NULL;char *result=calloc(1,1);size_t used=0;
 if(!result)return NULL;
 if(snd_device_name_hint(-1,"pcm",&hints)<0)return result;
 for(void **p=hints;*p;p++){
  char *name=snd_device_name_get_hint(*p,"NAME"),*io=snd_device_name_get_hint(*p,"IOID");
  if(name&&(!io||strcmp(io,"Output"))){
   size_t n=strlen(name);char *next=realloc(result,used+n+2);
   if(!next){free(name);free(io);free(result);snd_device_name_free_hint(hints);return NULL;}
   result=next;memcpy(result+used,name,n);used+=n;result[used++]='\n';result[used]=0;
  }
  free(name);free(io);
 }
 snd_device_name_free_hint(hints);return result;
}
*/
import "C"

import (
	"strings"
	"unsafe"
)

func ListNativeAudioDevices() []string {
	data := C.bk_alsa_inputs()
	if data == nil {
		return nil
	}
	defer C.free(unsafe.Pointer(data))
	result := []string{"default"}
	seen := map[string]bool{"default": true}
	for _, name := range strings.Split(C.GoString(data), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || name == "null" || seen[name] {
			continue
		}
		output := false
		for _, prefix := range []string{"dmix:", "front:", "rear:", "center_lfe:", "side:", "surround", "hdmi:", "iec958:"} {
			if strings.HasPrefix(name, prefix) {
				output = true
				break
			}
		}
		if !output {
			result = append(result, name)
			seen[name] = true
		}
	}
	return result
}
