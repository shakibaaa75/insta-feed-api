<?php
/**
 * Plugin Name: IG Feed Sync
 * Description: Syncs Instagram post links from your Go API twice a day, stores them in WordPress and shows them with [ig_feed count="9"].
 * Version: 1.0.0
 */
if (!defined('ABSPATH')) exit;

const IGFS_OPT  = 'igfs_settings';
const IGFS_LAST = 'igfs_last';
const IGFS_HOOK = 'igfs_sync';

/* ---------- Post type ---------- */
add_action('init', function () {
    register_post_type('ig_post', [
        'label'    => 'Instagram Posts',
        'public'   => false,
        'show_ui'  => true,
        'supports' => ['title'],
        'menu_icon' => 'dashicons-instagram',
    ]);
});

/* ---------- Schedule: twice a day ---------- */
register_activation_hook(__FILE__, function () {
    if (!wp_next_scheduled(IGFS_HOOK)) {
        wp_schedule_event(time() + 60, 'twicedaily', IGFS_HOOK);
    }
});
register_deactivation_hook(__FILE__, function () {
    wp_clear_scheduled_hook(IGFS_HOOK);
});
add_action(IGFS_HOOK, 'igfs_sync');

/* ---------- Settings ---------- */
function igfs_settings() {
    return wp_parse_args(get_option(IGFS_OPT, []), [
        'api_url' => '', 'api_key' => '', 'profile' => '', 'limit' => 100,
    ]);
}

add_action('admin_init', function () {
    register_setting('igfs_group', IGFS_OPT, ['sanitize_callback' => function ($in) {
        return [
            'api_url' => esc_url_raw($in['api_url'] ?? ''),
            'api_key' => sanitize_text_field($in['api_key'] ?? ''),
            'profile' => sanitize_text_field($in['profile'] ?? ''),
            'limit'   => max(1, min(500, (int) ($in['limit'] ?? 100))),
        ];
    }]);
});

add_action('admin_menu', function () {
    add_options_page('IG Feed Sync', 'IG Feed Sync', 'manage_options', 'igfs', 'igfs_page');
});

function igfs_page() {
    if (!current_user_can('manage_options')) return;
    $s    = igfs_settings();
    $last = get_option(IGFS_LAST);
    $next = wp_next_scheduled(IGFS_HOOK);
    ?>
    <div class="wrap">
        <h1>IG Feed Sync</h1>
        <form method="post" action="options.php">
            <?php settings_fields('igfs_group'); ?>
            <table class="form-table">
                <tr><th>API URL</th><td><input class="regular-text" name="<?php echo IGFS_OPT; ?>[api_url]" value="<?php echo esc_attr($s['api_url']); ?>" placeholder="https://your-api.example.com"></td></tr>
                <tr><th>API key</th><td><input class="regular-text" type="password" name="<?php echo IGFS_OPT; ?>[api_key]" value="<?php echo esc_attr($s['api_key']); ?>"></td></tr>
                <tr><th>Instagram username</th><td><input class="regular-text" name="<?php echo IGFS_OPT; ?>[profile]" value="<?php echo esc_attr($s['profile']); ?>"><p class="description">Optional. Leave empty to use the profile set on the API.</p></td></tr>
                <tr><th>Posts per sync</th><td><input type="number" min="1" max="500" name="<?php echo IGFS_OPT; ?>[limit]" value="<?php echo (int) $s['limit']; ?>"></td></tr>
            </table>
            <?php submit_button(); ?>
        </form>

        <form method="post" action="<?php echo esc_url(admin_url('admin-post.php')); ?>">
            <input type="hidden" name="action" value="igfs_sync_now">
            <?php wp_nonce_field('igfs_sync_now'); submit_button('Sync now', 'secondary'); ?>
        </form>

        <h2>Status</h2>
        <p>
            <?php if ($last) : ?>
                Last sync: <?php echo esc_html(wp_date('Y-m-d H:i', $last['time'])); ?> —
                <strong><?php echo $last['ok'] ? 'OK' : 'FAILED'; ?></strong>,
                <?php echo esc_html($last['msg']); ?> (new posts: <?php echo (int) $last['added']; ?>)
            <?php else : ?>
                No sync has run yet.
            <?php endif; ?>
            <br>Next automatic sync: <?php echo $next ? esc_html(wp_date('Y-m-d H:i', $next)) : 'not scheduled'; ?>
        </p>
        <p>Show the feed anywhere with <code>[ig_feed count="9"]</code>.</p>
    </div>
    <?php
}

add_action('admin_post_igfs_sync_now', function () {
    if (!current_user_can('manage_options')) wp_die('Not allowed');
    check_admin_referer('igfs_sync_now');
    igfs_sync();
    wp_safe_redirect(admin_url('options-general.php?page=igfs'));
    exit;
});

/* ---------- Sync from the Go API ---------- */
function igfs_done($ok, $msg, $added = 0) {
    update_option(IGFS_LAST, ['time' => time(), 'ok' => $ok, 'msg' => $msg, 'added' => $added], false);
    return $ok;
}

function igfs_sync() {
    $s = igfs_settings();
    if (!$s['api_url'] || !$s['api_key']) {
        return igfs_done(false, 'API URL and key are not set.');
    }

    $url = add_query_arg(
        ['limit' => (int) $s['limit'], 'profile' => $s['profile']],
        untrailingslashit($s['api_url']) . '/posts'
    );
    $res = wp_remote_get($url, ['timeout' => 20, 'headers' => ['X-API-Key' => $s['api_key']]]);

    // On any failure we stop here: posts already saved stay exactly as they are.
    if (is_wp_error($res)) return igfs_done(false, $res->get_error_message());
    $code = wp_remote_retrieve_response_code($res);
    if ($code !== 200) return igfs_done(false, "API returned HTTP $code");

    $data  = json_decode(wp_remote_retrieve_body($res), true);
    $posts = is_array($data) ? ($data['posts'] ?? []) : [];

    global $wpdb;
    $existing = array_flip($wpdb->get_col($wpdb->prepare(
        "SELECT meta_value FROM {$wpdb->postmeta} WHERE meta_key = %s", '_ig_shortcode'
    )));

    $added = 0;
    foreach (array_reverse($posts) as $p) { // oldest first
        $code = $p['shortcode'] ?? '';
        if (!preg_match('/^[A-Za-z0-9_-]+$/', $code) || isset($existing[$code])) continue;

        $ts  = strtotime($p['postedAt'] ?? '') ?: time();
        $gmt = gmdate('Y-m-d H:i:s', $ts);
        $id  = wp_insert_post([
            'post_type'     => 'ig_post',
            'post_status'   => 'publish',
            'post_title'    => $code,
            'post_date_gmt' => $gmt,
            'post_date'     => get_date_from_gmt($gmt),
        ]);
        if ($id && !is_wp_error($id)) {
            update_post_meta($id, '_ig_shortcode', $code);
            $existing[$code] = true;
            $added++;
        }
    }
    return igfs_done(true, 'fetched ' . count($posts) . ' posts', $added);
}

/* ---------- Frontend: [ig_feed count="9"] ---------- */
add_shortcode('ig_feed', function ($atts) {
    $a = shortcode_atts(['count' => 9], $atts);
    $q = new WP_Query([
        'post_type'      => 'ig_post',
        'post_status'    => 'publish',
        'posts_per_page' => max(1, (int) $a['count']),
        'orderby'        => 'date',
        'order'          => 'DESC',
        'no_found_rows'  => true,
    ]);

    $out = '<div class="ig-feed" style="display:grid;grid-template-columns:repeat(auto-fill,minmax(300px,1fr));gap:24px;">';
    while ($q->have_posts()) {
        $q->the_post();
        $code = get_post_meta(get_the_ID(), '_ig_shortcode', true);
        if (!$code) continue;
        $out .= '<div><blockquote class="instagram-media" data-instgrm-version="14" '
              . 'data-instgrm-permalink="https://www.instagram.com/p/' . esc_attr($code) . '/" '
              . 'style="max-width:540px;width:100%;margin:0 auto;"></blockquote></div>';
    }
    wp_reset_postdata();
    $out .= '</div><script async src="https://www.instagram.com/embed.js"></script>';
    return $out;
});
