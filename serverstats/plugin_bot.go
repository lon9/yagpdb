package serverstats

import (
	"fmt"
	"github.com/jonas747/dcmd"
	"github.com/jonas747/discordgo"
	"github.com/jonas747/yagpdb/bot"
	"github.com/jonas747/yagpdb/bot/eventsystem"
	"github.com/jonas747/yagpdb/commands"
	"github.com/jonas747/yagpdb/common"
	"github.com/mediocregopher/radix.v2/redis"
	log "github.com/sirupsen/logrus"
	"sync"
	"time"
)

func MarkGuildAsToBeChecked(rcli *redis.Client, guildID int64) {
	rcli.Cmd("SADD", "serverstats_active_guilds", guildID)
}

var (
	_ bot.BotInitHandler       = (*Plugin)(nil)
	_ bot.BotStopperHandler    = (*Plugin)(nil)
	_ commands.CommandProvider = (*Plugin)(nil)
)

func (p *Plugin) BotInit() {
	eventsystem.AddHandler(bot.RedisWrapper(HandleMemberAdd), eventsystem.EventGuildMemberAdd)
	eventsystem.AddHandler(bot.RedisWrapper(HandleMemberRemove), eventsystem.EventGuildMemberRemove)
	eventsystem.AddHandler(bot.RedisWrapper(HandleMessageCreate), eventsystem.EventMessageCreate)

	eventsystem.AddHandler(bot.RedisWrapper(HandlePresenceUpdate), eventsystem.EventPresenceUpdate)
	eventsystem.AddHandler(bot.RedisWrapper(HandleGuildCreate), eventsystem.EventGuildCreate)
	eventsystem.AddHandler(bot.RedisWrapper(HandleReady), eventsystem.EventReady)

	go UpdateStatsLoop()
}

func (p *Plugin) StopBot(wg *sync.WaitGroup) {
	stopStatsLoop <- wg
}

func (p *Plugin) AddCommands() {
	commands.AddRootCommands(&commands.YAGCommand{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryTool,
		Cooldown:      5,
		Name:          "Stats",
		Description:   "Shows server stats (if public stats are enabled)",
		RunFunc: func(data *dcmd.Data) (interface{}, error) {
			config, err := GetConfig(data.Context(), data.GS.ID)
			if err != nil {
				return "Failed retreiving guild config", err
			}

			if !config.Public {
				return fmt.Sprintf("Stats are set to private on this server, this can be changed in the control panel on <https://%s>", common.Conf.Host), nil
			}

			stats, err := RetrieveFullStats(data.Context().Value(commands.CtxKeyRedisClient).(*redis.Client), data.GS.ID)
			if err != nil {
				return "Error retrieving stats", err
			}

			total := int64(0)
			for _, c := range stats.ChannelsHour {
				total += c.Count
			}

			embed := &discordgo.MessageEmbed{
				Title:       "Server stats",
				Description: fmt.Sprintf("[Click here to open in browser](https://%s/public/%d/stats)", common.Conf.Host, data.GS.ID),
				Fields: []*discordgo.MessageEmbedField{
					&discordgo.MessageEmbedField{Name: "Members joined 24h", Value: fmt.Sprint(stats.JoinedDay), Inline: true},
					&discordgo.MessageEmbedField{Name: "Members Left 24h", Value: fmt.Sprint(stats.LeftDay), Inline: true},
					&discordgo.MessageEmbedField{Name: "Total Messages 24h", Value: fmt.Sprint(total), Inline: true},
					&discordgo.MessageEmbedField{Name: "Members Online", Value: fmt.Sprint(stats.Online), Inline: true},
					&discordgo.MessageEmbedField{Name: "Total Members", Value: fmt.Sprint(stats.TotalMembers), Inline: true},
				},
			}

			return embed, nil
		},
	})

}

func HandleReady(evt *eventsystem.EventData) {
	r := evt.Ready()
	client := bot.ContextRedis(evt.Context())

	for _, guild := range r.Guilds {
		if guild.Unavailable {
			continue
		}

		err := ApplyPresences(client, guild.ID, guild.Presences)
		if err != nil {
			log.WithError(err).Error("Failed applying presences")
		}
	}
}

func HandleGuildCreate(evt *eventsystem.EventData) {
	g := evt.GuildCreate()
	client := bot.ContextRedis(evt.Context())

	err := client.Cmd("SET", "guild_stats_num_members:"+discordgo.StrID(g.ID), g.MemberCount).Err
	if err != nil {
		log.WithError(err).Error("Failed Settings member count")
	}

	err = ApplyPresences(client, g.ID, g.Presences)
	if err != nil {
		log.WithError(err).Error("Failed applying presences")
	}
}

func HandleMemberAdd(evt *eventsystem.EventData) {
	g := evt.GuildMemberAdd()
	client := bot.ContextRedis(evt.Context())

	err := client.Cmd("ZADD", "guild_stats_members_joined_day:"+discordgo.StrID(g.GuildID), time.Now().Unix(), g.User.ID).Err
	if err != nil {
		log.WithError(err).Error("Failed adding member to stats")
	}

	err = client.Cmd("INCR", "guild_stats_num_members:"+discordgo.StrID(g.GuildID)).Err
	if err != nil {
		log.WithError(err).Error("Failed Increasing members")
	}
}

func HandlePresenceUpdate(evt *eventsystem.EventData) {
	p := evt.PresenceUpdate()
	client := bot.ContextRedis(evt.Context())

	if p.Status == "" { // Not a status update
		return
	}

	var err error
	if p.Status == "offline" {
		err = client.Cmd("SREM", "guild_stats_online:"+discordgo.StrID(p.GuildID), p.User.ID).Err
	} else {
		err = client.Cmd("SADD", "guild_stats_online:"+discordgo.StrID(p.GuildID), p.User.ID).Err
	}

	if err != nil {
		log.WithError(err).Error("Failed updating a presence")
	}
}

func HandleMemberRemove(evt *eventsystem.EventData) {
	g := evt.GuildMemberRemove()
	client := bot.ContextRedis(evt.Context())

	err := client.Cmd("ZADD", "guild_stats_members_left_day:"+discordgo.StrID(g.GuildID), time.Now().Unix(), g.User.ID).Err
	if err != nil {
		log.WithError(err).Error("Failed adding member to stats")
	}

	err = client.Cmd("DECR", "guild_stats_num_members:"+discordgo.StrID(g.GuildID)).Err
	if err != nil {
		log.WithError(err).Error("Failed decreasing members")
	}
}

func HandleMessageCreate(evt *eventsystem.EventData) {

	m := evt.MessageCreate()
	client := bot.ContextRedis(evt.Context())
	channel := bot.State.Channel(true, m.ChannelID)

	if channel == nil {
		log.WithField("channel", m.ChannelID).Warn("Channel not in state")
		return
	}

	if channel.IsPrivate() {
		return
	}

	config, err := GetConfig(evt.Context(), channel.Guild.ID)
	if err != nil {
		log.WithError(err).WithField("guild", channel.Guild.ID).Error("Failed retrieving config")
		return
	}

	if common.ContainsInt64Slice(config.ParsedChannels, channel.ID) {
		return
	}

	err = client.Cmd("ZADD", "guild_stats_msg_channel_day:"+channel.Guild.StrID(), time.Now().Unix(), channel.StrID()+":"+discordgo.StrID(m.ID)+":"+discordgo.StrID(m.Author.ID)).Err
	if err != nil {
		log.WithError(err).Error("Failed adding member to stats")
	}

	MarkGuildAsToBeChecked(client, channel.Guild.ID)
}

func ApplyPresences(client *redis.Client, guildID int64, presences []*discordgo.Presence) error {
	client.PipeAppend("DEL", "guild_stats_online:"+discordgo.StrID(guildID))
	count := 1
	for _, p := range presences {
		if p.Status == "offline" {
			continue
		}
		count++
		client.PipeAppend("SADD", "guild_stats_online:"+discordgo.StrID(guildID), p.User.ID)
	}

	_, err := common.GetRedisReplies(client, count)
	return err
}
