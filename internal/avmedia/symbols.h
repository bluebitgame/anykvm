#ifndef ANYKVM_AV_SYMBOLS_H
#define ANYKVM_AV_SYMBOLS_H
#define BM_SYMBOLS(X) \
 X(av_bsf_get_by_name,3) \
 X(av_bsf_alloc,3) \
 X(av_bsf_init,3) \
 X(av_bsf_free,3) \
 X(av_bsf_send_packet,3) \
 X(av_bsf_receive_packet,3) \
 X(av_bsf_flush,3) \
 X(avcodec_parameters_copy,3) \
 X(avcodec_find_decoder,3) \
 X(sws_getCoefficients,2) \
 X(sws_setColorspaceDetails,2) \
 X(avutil_version,0) \
 X(swresample_version,1) \
 X(swscale_version,2) \
 X(avdevice_version,5) \
 X(av_mallocz,0) \
 X(av_buffer_unref,0) \
 X(av_channel_layout_copy,0) \
 X(av_channel_layout_default,0) \
 X(av_dict_free,0) \
 X(av_dict_set,0) \
 X(av_find_best_stream,4) \
 X(av_find_input_format,4) \
 X(av_frame_alloc,0) \
 X(av_frame_copy_props,0) \
 X(av_frame_free,0) \
 X(av_frame_get_buffer,0) \
 X(av_frame_unref,0) \
 X(av_get_pix_fmt,0) \
 X(av_hwdevice_ctx_create,0) \
 X(av_hwframe_ctx_alloc,0) \
 X(av_hwframe_ctx_init,0) \
 X(av_hwframe_get_buffer,0) \
 X(av_hwframe_transfer_data,0) \
 X(av_image_fill_arrays,0) \
 X(av_image_fill_black,0) \
 X(av_image_get_buffer_size,0) \
 X(av_log_default_callback,0) \
 X(av_log_set_callback,0) \
 X(av_new_packet,3) \
 X(av_packet_alloc,3) \
 X(av_packet_clone,3) \
 X(av_packet_move_ref,3) \
 X(av_packet_free,3) \
 X(av_packet_unref,3) \
 X(av_read_frame,4) \
 X(avio_find_protocol_name,4) \
 X(av_strerror,0) \
 X(avcodec_alloc_context3,3) \
 X(avcodec_find_decoder_by_name,3) \
 X(avcodec_find_encoder_by_name,3) \
 X(avcodec_free_context,3) \
 X(avcodec_open2,3) \
 X(avcodec_parameters_to_context,3) \
 X(avcodec_receive_frame,3) \
 X(avcodec_receive_packet,3) \
 X(avcodec_send_frame,3) \
 X(avcodec_send_packet,3) \
 X(avcodec_version,3) \
 X(avdevice_register_all,5) \
 X(avformat_alloc_context,4) \
 X(avformat_close_input,4) \
 X(avformat_open_input,4) \
 X(avformat_find_stream_info,4) \
 X(avformat_seek_file,4) \
 X(avformat_version,4) \
 X(swr_alloc_set_opts2,1) \
 X(swr_convert,1) \
 X(swr_free,1) \
 X(swr_get_out_samples,1) \
 X(swr_init,1) \
 X(sws_freeContext,2) \
 X(sws_getCachedContext,2) \
 X(sws_scale,2)

#define DECLARE(name,lib) static __typeof__(&name) p_##name;
BM_SYMBOLS(DECLARE)
#undef DECLARE
#define av_bsf_get_by_name (*p_av_bsf_get_by_name)
#define av_bsf_alloc (*p_av_bsf_alloc)
#define av_bsf_init (*p_av_bsf_init)
#define av_bsf_free (*p_av_bsf_free)
#define av_bsf_send_packet (*p_av_bsf_send_packet)
#define av_bsf_receive_packet (*p_av_bsf_receive_packet)
#define av_bsf_flush (*p_av_bsf_flush)
#define avcodec_parameters_copy (*p_avcodec_parameters_copy)
#define avcodec_find_decoder (*p_avcodec_find_decoder)
#define av_buffer_unref (*p_av_buffer_unref)
#define av_channel_layout_copy (*p_av_channel_layout_copy)
#define av_channel_layout_default (*p_av_channel_layout_default)
#define av_dict_free (*p_av_dict_free)
#define av_dict_set (*p_av_dict_set)
#define av_find_best_stream (*p_av_find_best_stream)
#define av_find_input_format (*p_av_find_input_format)
#define av_frame_alloc (*p_av_frame_alloc)
#define av_frame_copy_props (*p_av_frame_copy_props)
#define av_frame_free (*p_av_frame_free)
#define av_frame_get_buffer (*p_av_frame_get_buffer)
#define av_frame_unref (*p_av_frame_unref)
#define av_get_pix_fmt (*p_av_get_pix_fmt)
#define av_hwdevice_ctx_create (*p_av_hwdevice_ctx_create)
#define av_hwframe_ctx_alloc (*p_av_hwframe_ctx_alloc)
#define av_hwframe_ctx_init (*p_av_hwframe_ctx_init)
#define av_hwframe_get_buffer (*p_av_hwframe_get_buffer)
#define av_hwframe_transfer_data (*p_av_hwframe_transfer_data)
#define av_image_fill_arrays (*p_av_image_fill_arrays)
#define av_image_fill_black (*p_av_image_fill_black)
#define av_image_get_buffer_size (*p_av_image_get_buffer_size)
#define av_log_default_callback (*p_av_log_default_callback)
#define av_log_set_callback (*p_av_log_set_callback)
#define av_new_packet (*p_av_new_packet)
#define av_packet_alloc (*p_av_packet_alloc)
#define av_packet_clone (*p_av_packet_clone)
#define av_packet_move_ref (*p_av_packet_move_ref)
#define av_packet_free (*p_av_packet_free)
#define av_packet_unref (*p_av_packet_unref)
#define av_read_frame (*p_av_read_frame)
#define avio_find_protocol_name (*p_avio_find_protocol_name)
#define av_strerror (*p_av_strerror)
#define avcodec_alloc_context3 (*p_avcodec_alloc_context3)
#define avcodec_find_decoder_by_name (*p_avcodec_find_decoder_by_name)
#define avcodec_find_encoder_by_name (*p_avcodec_find_encoder_by_name)
#define avcodec_free_context (*p_avcodec_free_context)
#define avcodec_open2 (*p_avcodec_open2)
#define avcodec_parameters_to_context (*p_avcodec_parameters_to_context)
#define avcodec_receive_frame (*p_avcodec_receive_frame)
#define avcodec_receive_packet (*p_avcodec_receive_packet)
#define avcodec_send_frame (*p_avcodec_send_frame)
#define avcodec_send_packet (*p_avcodec_send_packet)
#define sws_getCoefficients (*p_sws_getCoefficients)
#define sws_setColorspaceDetails (*p_sws_setColorspaceDetails)
#define avutil_version (*p_avutil_version)
#define swresample_version (*p_swresample_version)
#define swscale_version (*p_swscale_version)
#define avdevice_version (*p_avdevice_version)
#define av_mallocz (*p_av_mallocz)
#define avcodec_version (*p_avcodec_version)
#define avdevice_register_all (*p_avdevice_register_all)
#define avformat_alloc_context (*p_avformat_alloc_context)
#define avformat_close_input (*p_avformat_close_input)
#define avformat_open_input (*p_avformat_open_input)
#define avformat_find_stream_info (*p_avformat_find_stream_info)
#define avformat_seek_file (*p_avformat_seek_file)
#define avformat_version (*p_avformat_version)
#define swr_alloc_set_opts2 (*p_swr_alloc_set_opts2)
#define swr_convert (*p_swr_convert)
#define swr_free (*p_swr_free)
#define swr_get_out_samples (*p_swr_get_out_samples)
#define swr_init (*p_swr_init)
#define sws_freeContext (*p_sws_freeContext)
#define sws_getCachedContext (*p_sws_getCachedContext)
#define sws_scale (*p_sws_scale)
#endif
