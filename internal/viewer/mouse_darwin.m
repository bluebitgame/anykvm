//go:build viewer

/*
 * AnyKVM — GCMouseInput raw delta capture (macOS 11+).
 * External USB/BT mice only; built-in trackpad does not appear as GCMouse.
 */
#import <Foundation/Foundation.h>
#import <GameController/GameController.h>
#include <pthread.h>

typedef void (*anykvm_gc_notify_fn)(void);

static int g_active;
static int g_mouse_count;
static anykvm_gc_notify_fn g_notify;
static pthread_mutex_t g_mu = PTHREAD_MUTEX_INITIALIZER;
static float g_pdx;
static float g_pdy;
static id g_connectObs;
static id g_disconnectObs;
static id g_currentObs;

static void resync_mice(void)
{
	if (@available(macOS 11.0, *)) {
		int count = 0;

		for (GCMouse *mouse in [GCMouse mice]) {
			if (mouse.mouseInput) {
				count++;
			}
		}
		GCMouse *current = [GCMouse current];
		if (count == 0 && current && current.mouseInput) {
			count = 1;
		}
		g_mouse_count = count;
	}
}

static void notify_go(void)
{
	anykvm_gc_notify_fn n = g_notify;
	if (n) {
		n();
	}
}

static void add_delta(float dx, float dy)
{
	if (!g_active || (dx == 0.0f && dy == 0.0f)) {
		return;
	}
	pthread_mutex_lock(&g_mu);
	g_pdx += dx;
	g_pdy += dy;
	pthread_mutex_unlock(&g_mu);
	notify_go();
}

static void setup_mouse(GCMouse *mouse)
{
	GCMouseInput *input = mouse.mouseInput;
	if (!input) {
		return;
	}
	input.mouseMovedHandler = ^(GCMouseInput *in, float dx, float dy) {
		(void)in;
		/* GCMouse +Y is up; SDL/HID +Y is down. */
		add_delta(dx, -dy);
	};
}

static void teardown_mouse(GCMouse *mouse)
{
	if (mouse.mouseInput) {
		mouse.mouseInput.mouseMovedHandler = nil;
	}
}

static GCMouse *active_mouse(void)
{
	if (@available(macOS 11.0, *)) {
		GCMouse *active = [GCMouse current];
		if (active && active.mouseInput) {
			return active;
		}
		for (GCMouse *mouse in [GCMouse mice]) {
			if (mouse.mouseInput) {
				return mouse;
			}
		}
	}
	return nil;
}

static void attach_all_mice(void)
{
	if (@available(macOS 11.0, *)) {
		for (GCMouse *mouse in [GCMouse mice]) {
			teardown_mouse(mouse);
		}
		GCMouse *active = active_mouse();
		if (active) {
			setup_mouse(active);
		}
		resync_mice();
	}
}

static void mouse_changed(NSNotification *note)
{
	(void)note;
	dispatch_async(dispatch_get_main_queue(), ^{
		attach_all_mice();
	});
}

void anykvm_gc_set_notify(anykvm_gc_notify_fn fn)
{
	g_notify = fn;
}

int anykvm_gc_start(void)
{
	g_mouse_count = 0;
	if (@available(macOS 11.0, *)) {
		NSNotificationCenter *nc = [NSNotificationCenter defaultCenter];
		g_connectObs = [nc addObserverForName:GCMouseDidConnectNotification
					       object:nil
						queue:nil
					   usingBlock:^(NSNotification *note) {
						   mouse_changed(note);
					   }];
		g_disconnectObs = [nc addObserverForName:GCMouseDidDisconnectNotification
						  object:nil
						   queue:nil
					     usingBlock:^(NSNotification *note) {
						     mouse_changed(note);
					     }];
		g_currentObs = [nc addObserverForName:GCMouseDidBecomeCurrentNotification
					       object:nil
						queue:nil
					   usingBlock:^(NSNotification *note) {
						   mouse_changed(note);
					   }];
		attach_all_mice();
	}
	return g_mouse_count;
}

void anykvm_gc_resync(void)
{
	attach_all_mice();
}

void anykvm_gc_stop(void)
{
	if (@available(macOS 11.0, *)) {
		NSNotificationCenter *nc = [NSNotificationCenter defaultCenter];
		if (g_connectObs) {
			[nc removeObserver:g_connectObs];
			g_connectObs = nil;
		}
		if (g_disconnectObs) {
			[nc removeObserver:g_disconnectObs];
			g_disconnectObs = nil;
		}
		if (g_currentObs) {
			[nc removeObserver:g_currentObs];
			g_currentObs = nil;
		}
		for (GCMouse *mouse in [GCMouse mice]) {
			teardown_mouse(mouse);
		}
	}
	g_notify = NULL;
	g_active = 0;
	g_mouse_count = 0;
	pthread_mutex_lock(&g_mu);
	g_pdx = g_pdy = 0;
	pthread_mutex_unlock(&g_mu);
}

void anykvm_gc_set_active(int on)
{
	g_active = on ? 1 : 0;
	if (!g_active) {
		pthread_mutex_lock(&g_mu);
		g_pdx = g_pdy = 0;
		pthread_mutex_unlock(&g_mu);
	}
}

int anykvm_gc_drain(float *dx, float *dy)
{
	pthread_mutex_lock(&g_mu);
	*dx = g_pdx;
	*dy = g_pdy;
	g_pdx = g_pdy = 0;
	pthread_mutex_unlock(&g_mu);
	if (*dx == 0.0f && *dy == 0.0f) {
		return 0;
	}
	return 1;
}

int anykvm_gc_mouse_count(void)
{
	return g_mouse_count;
}
