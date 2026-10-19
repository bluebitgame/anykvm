#include "../AnyKVMProtocol/src/AnyKVMDevice.h"
#include <assert.h>

static uint8_t replies[16][5];
static int count;
static uint8_t mode_result;
static void reply(void *ctx, uint8_t cmd, const uint8_t *data) {
  (void)ctx; assert(count < 16);
  replies[count][0] = cmd; memcpy(replies[count++] + 1, data, 4);
}
static uint8_t status(void *ctx) { (void)ctx; return 4; }
static uint8_t mode(void *ctx, bool absolute) { (void)ctx; (void)absolute; return mode_result; }
static void keyboard(void *ctx, const uint8_t *r) { (void)ctx; (void)r; }
static void mouse(void *ctx, bool absolute, const uint8_t *r, size_t n) {
  (void)ctx; (void)r; assert(n == (absolute ? 6 : 4));
}
static void command(bk_device *d, uint8_t cmd, uint8_t value) {
  uint8_t data[4] = {value,0,0,0}; count = 0; bk_command(d,cmd,data);
}
int main(void) {
  bk_device d = {0};
  d.capabilities = BK_KEYBOARD | BK_RELATIVE | BK_ABSOLUTE | BK_USB;
  d.model = BK_MODEL_PICO; d.reply = reply; d.status = status; d.mode = mode;
  d.input.keyboard = keyboard; d.input.mouse = mouse;
  command(&d,0x22,0);
  assert(count == 4 && replies[0][0] == 0x83 && replies[0][1] == 1 && replies[0][2] == 0x27);
  assert(replies[1][0] == 0x80 && replies[1][1] == 0);
  assert(replies[2][0] == 0x81 && replies[2][1] == 4);
  assert(replies[3][0] == 0x84 && replies[3][1] == 0x22 && replies[3][2] == BK_OK);
  command(&d,0x20,0); assert(replies[count-1][2] == BK_UNSUPPORTED);
  command(&d,0x04,8); assert(d.input.absolute && replies[2][1] == 0x24);
  mode_result = BK_INTERNAL;
  command(&d,0x04,16); assert(d.input.absolute && replies[count-1][2] == BK_INTERNAL);
  command(&d,0x04,9); assert(replies[count-1][2] == BK_INVALID_ARGUMENT);
  command(&d,0xfe,0); assert(count == 1 && replies[0][2] == BK_INVALID_COMMAND);
  d.capabilities |= BK_PROFILES; d.slots = 1;
  command(&d,0x21,1); assert(replies[count-1][2] == BK_INVALID_ARGUMENT);
  uint8_t key[4] = {0xe7,1,0,0}; bk_input_command(&d.input,0x11,key);
  d.input.buttons = 7;
  command(&d, BK_CMD_SELECT_TARGET, 1);
  assert(count == 1 && replies[0][2] == BK_UNSUPPORTED);
  assert(d.input.keys[0] == 0x80 && d.input.buttons == 7);
  command(&d,0x10,0);
  assert(d.input.keys[0] == 0 && d.input.buttons == 0);
  uint8_t bad[4] = {0x80,0,0,0};
  assert(bk_input_command(&d.input,0x12,bad) == BK_INVALID_ARGUMENT);
}
