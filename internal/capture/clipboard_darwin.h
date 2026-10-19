#include <stdint.h>
#include <stddef.h>
int bk_clipboard_read(char **text, size_t *length, uint64_t *revision);
int bk_clipboard_write(const char *text, size_t length, uint64_t *revision);
int bk_clipboard_read_rich(char **html, size_t *html_length, char **rtf, size_t *rtf_length, uint64_t *revision);
int bk_clipboard_write_rich(const char *html, size_t html_length, const char *rtf, size_t rtf_length, uint64_t *revision);
int bk_clipboard_read_image(char **png, size_t *length, uint64_t *revision);
int bk_clipboard_write_image(const char *png, size_t length, uint64_t *revision);
int bk_clipboard_read_files(char **json, size_t *length, uint64_t *revision);
int bk_clipboard_publish_files(const char *json, size_t length, uint64_t offer,
    int lock_fd, uint64_t expected_revision, uint64_t *revision);
void bk_clipboard_retire_files(uint64_t offer);
void bk_clipboard_clear_file_offer(uint64_t offer);
void bk_clipboard_shutdown_files(void);
int bk_clipboard_materialize(uint64_t offer);
int bk_clipboard_file_offer_ready(uint64_t offer);
