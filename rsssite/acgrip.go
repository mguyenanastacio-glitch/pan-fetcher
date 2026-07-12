package rsssite

import "github.com/mmcdole/gofeed"

type Acgrip struct{}

func (a *Acgrip) GetMagnet(item *gofeed.Item) string {
	return GetMagnetByEnclosure(item)
}

func (a *Acgrip) GetMagnetItem(item *gofeed.Item) MagnetItem {
	return MagnetItem{
		Title:       item.Title,
		Link:        item.Link,
		Magnet:      a.GetMagnet(item),
		Description: item.Description,
		Content:     item.Content,
	}
}